/*
Copyright 2014 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tools

import (
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"
	"github.com/coreos/go-etcd/etcd"
	"github.com/golang/glog"
)

// FilterFunc is a predicate which takes an API object and returns true
// iff the object should remain in the set.
type FilterFunc func(obj runtime.Object) bool

// Everything is a FilterFunc which accepts all objects.
func Everything(runtime.Object) bool {
	return true
}

// WatchList begins watching the specified key's items. Items are decoded into
// API objects, and any items passing 'filter' are sent down the returned
// watch.Interface. resourceVersion may be used to specify what version to begin
// watching (e.g., for reconnecting without missing any updates).
func (h *EtcdHelper) WatchList(key string, resourceVersion uint64, filter FilterFunc) (watch.Interface, error) {
	w := newEtcdWatcher(true, filter, h.Codec, h.ResourceVersioner, nil)
	go w.etcdWatch(h.Client, key, resourceVersion)
	return w, nil
}

// Watch begins watching the specified key. Events are decoded into
// API objects and sent down the returned watch.Interface.
func (h *EtcdHelper) Watch(key string, resourceVersion uint64) (watch.Interface, error) {
	return h.WatchAndTransform(key, resourceVersion, nil)
}

// WatchAndTransform begins watching the specified key. Events are decoded into
// API objects and sent down the returned watch.Interface. If the transform
// function is provided, the value decoded from etcd will be passed to the function
// prior to being returned.
//
// The transform function can be used to populate data not available to etcd, or to
// change or wrap the serialized etcd object.
//
//   startTime := time.Now()
//   helper.WatchAndTransform(key, version, func(input runtime.Object) (runtime.Object, error) {
//     value := input.(TimeAwareValue)
//     value.Since = startTime
//     return value, nil
//   })
//
func (h *EtcdHelper) WatchAndTransform(key string, resourceVersion uint64, transform TransformFunc) (watch.Interface, error) {
	w := newEtcdWatcher(false, Everything, h.Codec, h.ResourceVersioner, transform)
	go w.etcdWatch(h.Client, key, resourceVersion)
	return w, <-w.immediateError
}

// TransformFunc attempts to convert an object to another object for use with a watcher.
type TransformFunc func(runtime.Object) (runtime.Object, error)

// etcdWatcher converts a native etcd watch to a watch.Interface.
type etcdWatcher struct {
	encoding  runtime.Codec
	versioner runtime.ResourceVersioner
	transform TransformFunc

	list   bool // If we're doing a recursive watch, should be true.
	filter FilterFunc

	etcdIncoming  chan *etcd.Response
	etcdStop      chan bool
	etcdCallEnded chan struct{}

	// etcdWatch will send an error down this channel if the Watch fails.
	// Otherwise, a nil will be sent down this channel watchWaitDuration
	// after the watch starts.
	immediateError chan error

	outgoing chan watch.Event
	userStop chan struct{}
	stopped  bool
	stopLock sync.Mutex

	// Injectable for testing. Send the event down the outgoing channel.
	emit func(watch.Event)
}

// watchWaitDuration is the amount of time to wait for an error from watch.
const watchWaitDuration = 100 * time.Millisecond

// newEtcdWatcher returns a new etcdWatcher; if list is true, watch sub-nodes.  If you provide a transform
// and a versioner, the versioner must be able to handle the objects that transform creates.
func newEtcdWatcher(list bool, filter FilterFunc, encoding runtime.Codec, versioner runtime.ResourceVersioner, transform TransformFunc) *etcdWatcher {
	w := &etcdWatcher{
		encoding:       encoding,
		versioner:      versioner,
		transform:      transform,
		list:           list,
		filter:         filter,
		etcdIncoming:   make(chan *etcd.Response),
		etcdStop:       make(chan bool),
		etcdCallEnded:  make(chan struct{}),
		immediateError: make(chan error),
		outgoing:       make(chan watch.Event),
		userStop:       make(chan struct{}),
	}
	w.emit = func(e watch.Event) { w.outgoing <- e }
	go w.translate()
	return w
}

// etcdWatch calls etcd's Watch function, and handles any errors. Meant to be called
// as a goroutine. Will either send an error over w.immediateError if Watch fails, or in 100ms will
func (w *etcdWatcher) etcdWatch(client EtcdGetSet, key string, resourceVersion uint64) {
	defer util.HandleCrash()
	defer close(w.etcdCallEnded)
	go func() {
		// This is racy; assume that Watch will fail within 100ms if it is going to fail.
		// It's still more useful than blocking until the first result shows up.
		// Trying to detect the 401: watch window expired error.
		<-time.After(watchWaitDuration)
		w.immediateError <- nil
	}()
	if resourceVersion == 0 {
		latest, ok := etcdGetInitialWatchState(client, key, w.list, w.etcdIncoming)
		if !ok {
			return
		}
		resourceVersion = latest + 1
	}
	_, err := client.Watch(key, resourceVersion, w.list, w.etcdIncoming, w.etcdStop)
	if err != etcd.ErrWatchStoppedByUser {
		glog.Errorf("etcd.Watch stopped unexpectedly: %v (%#v)", err, key)
		w.immediateError <- err
	}
}

// etcdGetInitialWatchState turns an etcd Get request into a watch equivalent
func etcdGetInitialWatchState(client EtcdGetSet, key string, recursive bool, incoming chan<- *etcd.Response) (resourceVersion uint64, success bool) {
	success = true

	resp, err := client.Get(key, false, recursive)
	if err != nil {
		if !IsEtcdNotFound(err) {
			glog.Errorf("watch was unable to retrieve the current index for the provided key: %v (%#v)", err, key)
			success = false
			return
		}
		if index, ok := etcdErrorIndex(err); ok {
			resourceVersion = index
		}
		return
	}
	resourceVersion = resp.EtcdIndex
	convertRecursiveResponse(resp.Node, resp, incoming)
	return
}

// convertRecursiveResponse turns a recursive get response from etcd into individual response objects
// by copying the original response.  This emulates the behavior of a recursive watch.
func convertRecursiveResponse(node *etcd.Node, response *etcd.Response, incoming chan<- *etcd.Response) {
	if node.Dir {
		for i := range node.Nodes {
			convertRecursiveResponse(node.Nodes[i], response, incoming)
		}
		return
	}
	copied := *response
	copied.Action = "get"
	copied.Node = node
	incoming <- &copied
}

// translate pulls stuff from etcd, convert, and push out the outgoing channel. Meant to be
// called as a goroutine.
func (w *etcdWatcher) translate() {
	defer close(w.outgoing)
	defer util.HandleCrash()

	for {
		select {
		case <-w.etcdCallEnded:
			return
		case <-w.userStop:
			w.etcdStop <- true
			return
		case res, ok := <-w.etcdIncoming:
			if !ok {
				return
			}
			w.sendResult(res)
		}
	}
}

func (w *etcdWatcher) decodeObject(data []byte, index uint64) (runtime.Object, error) {
	obj, err := w.encoding.Decode(data)
	if err != nil {
		return nil, err
	}

	// ensure resource version is set on the object we load from etcd
	if w.versioner != nil {
		if err := w.versioner.SetResourceVersion(obj, index); err != nil {
			glog.Errorf("failure to version api object (%d) %#v: %v", index, obj, err)
		}
	}

	// perform any necessary transformation
	if w.transform != nil {
		obj, err = w.transform(obj)
		if err != nil {
			glog.Errorf("failure to transform api object %#v: %v", obj, err)
			return nil, err
		}
	}

	return obj, nil
}

func (w *etcdWatcher) sendAdd(res *etcd.Response) {
	if res.Node == nil {
		glog.Errorf("unexpected nil node: %#v", res)
		return
	}
	data := []byte(res.Node.Value)
	obj, err := w.decodeObject(data, res.Node.ModifiedIndex)
	if err != nil {
		glog.Errorf("failure to decode api object: '%v' from %#v %#v", string(data), res, res.Node)
		// TODO: expose an error through watch.Interface?
		// Ignore this value. If we stop the watch on a bad value, a client that uses
		// the resourceVersion to resume will never be able to get past a bad value.
		return
	}
	if !w.filter(obj) {
		return
	}
	action := watch.Added
	if res.Node.ModifiedIndex != res.Node.CreatedIndex {
		action = watch.Modified
	}
	w.emit(watch.Event{
		Type:   action,
		Object: obj,
	})
}

func (w *etcdWatcher) sendModify(res *etcd.Response) {
	if res.Node == nil {
		glog.Errorf("unexpected nil node: %#v", res)
		return
	}
	curData := []byte(res.Node.Value)
	curObj, err := w.decodeObject(curData, res.Node.ModifiedIndex)
	if err != nil {
		glog.Errorf("failure to decode api object: '%v' from %#v %#v", string(curData), res, res.Node)
		// TODO: expose an error through watch.Interface?
		// Ignore this value. If we stop the watch on a bad value, a client that uses
		// the resourceVersion to resume will never be able to get past a bad value.
		return
	}
	curObjPasses := w.filter(curObj)
	oldObjPasses := false
	var oldObj runtime.Object
	if res.PrevNode != nil && res.PrevNode.Value != "" {
		// Ignore problems reading the old object.
		if oldObj, err = w.decodeObject([]byte(res.PrevNode.Value), res.PrevNode.ModifiedIndex); err == nil {
			oldObjPasses = w.filter(oldObj)
		}
	}
	// Some changes to an object may cause it to start or stop matching a filter.
	// We need to report those as adds/deletes. So we have to check both the previous
	// and current value of the object.
	switch {
	case curObjPasses && oldObjPasses:
		w.emit(watch.Event{
			Type:   watch.Modified,
			Object: curObj,
		})
	case curObjPasses && !oldObjPasses:
		w.emit(watch.Event{
			Type:   watch.Added,
			Object: curObj,
		})
	case !curObjPasses && oldObjPasses:
		w.emit(watch.Event{
			Type:   watch.Deleted,
			Object: oldObj,
		})
	}
	// Do nothing if neither new nor old object passed the filter.
}

func (w *etcdWatcher) sendDelete(res *etcd.Response) {
	if res.PrevNode == nil {
		glog.Errorf("unexpected nil prev node: %#v", res)
		return
	}
	data := []byte(res.PrevNode.Value)
	index := res.PrevNode.ModifiedIndex
	if res.Node != nil {
		// Note that this sends the *old* object with the etcd index for the time at
		// which it gets deleted. This will allow users to restart the watch at the right
		// index.
		index = res.Node.ModifiedIndex
	}
	obj, err := w.decodeObject(data, index)
	if err != nil {
		glog.Errorf("failure to decode api object: '%v' from %#v %#v", string(data), res, res.PrevNode)
		// TODO: expose an error through watch.Interface?
		// Ignore this value. If we stop the watch on a bad value, a client that uses
		// the resourceVersion to resume will never be able to get past a bad value.
		return
	}
	if !w.filter(obj) {
		return
	}
	w.emit(watch.Event{
		Type:   watch.Deleted,
		Object: obj,
	})
}

func (w *etcdWatcher) sendResult(res *etcd.Response) {
	switch res.Action {
	case "create", "get":
		w.sendAdd(res)
	case "set", "compareAndSwap":
		w.sendModify(res)
	case "delete":
		w.sendDelete(res)
	default:
		glog.Errorf("unknown action: %v", res.Action)
	}
}

// ResultChan implements watch.Interface.
func (w *etcdWatcher) ResultChan() <-chan watch.Event {
	return w.outgoing
}

// Stop implements watch.Interface.
func (w *etcdWatcher) Stop() {
	w.stopLock.Lock()
	defer w.stopLock.Unlock()
	// Prevent double channel closes.
	if !w.stopped {
		w.stopped = true
		close(w.userStop)
	}
}
