package ram

import (
	"context"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/client-go/tools/cache"

	oknstorage "okn/pkg/storage"
)

// testEvent implements InternalEvent.
type testEvent struct {
	Type            watch.EventType
	Object          runtime.Object
	ObjLabels       labels.Set
	ObjFields       fields.Set
	PrevObject      runtime.Object
	PrevObjLabels   labels.Set
	PrevObjFields   fields.Set
	Key             string
	ResourceVersion uint64
}

func (event *testEvent) ToWatchEvent(selectors *oknstorage.Selectors) *watch.Event {
	filter := func(s *oknstorage.Selectors, key string, labels labels.Set, fields fields.Set) bool {
		if s.Key != "" && key != s.Key {
			return false
		}
		if s.Label.Empty() && s.Field.Empty() {
			return true
		}
		if !s.Label.Matches(labels) {
			return false
		}
		return s.Field.Matches(fields)
	}

	curObjPasses := event.Type != watch.Deleted && filter(selectors, event.Key, event.ObjLabels, event.ObjFields)
	oldObjPasses := false
	if event.PrevObject != nil {
		oldObjPasses = filter(selectors, event.Key, event.PrevObjLabels, event.PrevObjFields)
	}
	if !curObjPasses && !oldObjPasses {
		// Watcher is not interested in that object.
		return nil
	}

	switch {
	case curObjPasses && !oldObjPasses:
		return &watch.Event{Type: watch.Added, Object: event.Object.DeepCopyObject()}
	case curObjPasses && oldObjPasses:
		return &watch.Event{Type: watch.Modified, Object: event.Object.DeepCopyObject()}
	case !curObjPasses && oldObjPasses:
		// return a delete event with the previous object content
		return &watch.Event{Type: watch.Deleted, Object: event.PrevObject.DeepCopyObject()}
	}
	return nil
}

func (event *testEvent) GetResourceVersion() uint64 {
	return event.ResourceVersion
}

// testGenEvent generates *testEvent
func testGenEvent(key string, prevObj, obj runtime.Object, resourceVersion uint64) (oknstorage.InternalEvent, error) {
	if reflect.DeepEqual(prevObj, obj) {
		return nil, nil
	}
	event := &testEvent{Key: key, ResourceVersion: resourceVersion}
	if prevObj != nil {
		prevObjLabels, prevObjFields, err := storage.DefaultClusterScopedAttr(prevObj)
		if err != nil {
			return nil, err
		}
		event.PrevObject = prevObj
		event.PrevObjLabels = prevObjLabels
		event.PrevObjFields = prevObjFields
	}
	if obj != nil {
		objLabels, objFields, err := storage.DefaultClusterScopedAttr(obj)
		if err != nil {
			return nil, err
		}
		event.Object = obj
		event.ObjLabels = objLabels
		event.ObjFields = objFields
	}
	if prevObj == nil && obj != nil {
		event.Type = watch.Added
	} else if prevObj != nil && obj == nil {
		event.Type = watch.Deleted
	} else {
		event.Type = watch.Modified
	}
	return event, nil
}

func TestRamStoreCRUD(t *testing.T) {
	key := "pod1"
	testCases := []struct {
		// The operations that will be executed on the storage
		operations func(*store)
		// The object expected to be got by the key
		expected runtime.Object
	}{
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key, Labels: map[string]string{"app": "nginx1"}}})
			},
			expected: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key, Labels: map[string]string{"app": "nginx1"}}},
		},
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key, Labels: map[string]string{"app": "nginx1"}}})
				store.Update(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key, Labels: map[string]string{"app": "nginx2"}}})
			},
			expected: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key, Labels: map[string]string{"app": "nginx2"}}},
		},
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key, Labels: map[string]string{"app": "nginx1"}}})
				store.Update(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key, Labels: map[string]string{"app": "nginx2"}}})
				store.Delete(key)
			},
			expected: nil,
		},
	}
	for i, testCase := range testCases {
		store := NewStore(cache.MetaNamespaceKeyFunc, cache.Indexers{}, testGenEvent)

		testCase.operations(store)
		obj, _, err := store.Get(key)
		if err != nil {
			t.Errorf("%d: failed to get object: %v", i, err)
		}
		if !reflect.DeepEqual(obj, testCase.expected) {
			t.Errorf("%d: get unexpected object: %v", i, obj)
		}
	}
}

func TestRamStoreGetByIndex(t *testing.T) {
	indexName := "nodeName"
	indexKey := "node1"
	indexers := cache.Indexers{
		indexName: func(obj interface{}) ([]string, error) {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				return []string{}, nil
			}
			if len(pod.Spec.NodeName) == 0 {
				return []string{}, nil
			}
			return []string{pod.Spec.NodeName}, nil
		},
	}
	testCases := []struct {
		// The operations that will be executed on the storage
		operations func(*store)
		// The objects expected to be got by the indexName and indexKey
		expected []runtime.Object
	}{
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}, Spec: v1.PodSpec{NodeName: indexKey}})
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"app": "nginx2"}}, Spec: v1.PodSpec{NodeName: indexKey}})
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod3", Labels: map[string]string{"app": "nginx3"}}, Spec: v1.PodSpec{NodeName: "othernode"}})
			},
			expected: []runtime.Object{
				&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}, Spec: v1.PodSpec{NodeName: indexKey}},
				&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"app": "nginx2"}}, Spec: v1.PodSpec{NodeName: indexKey}},
			},
		},
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}, Spec: v1.PodSpec{NodeName: indexKey}})
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"app": "nginx2"}}, Spec: v1.PodSpec{NodeName: indexKey}})
				store.Delete("pod2")
			},
			expected: []runtime.Object{
				&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}, Spec: v1.PodSpec{NodeName: indexKey}},
			},
		},
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}, Spec: v1.PodSpec{NodeName: indexKey}})
				store.Update(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx2"}}, Spec: v1.PodSpec{NodeName: indexKey}})
			},
			expected: []runtime.Object{
				&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx2"}}, Spec: v1.PodSpec{NodeName: indexKey}},
			},
		},
	}
	for i, testCase := range testCases {
		store := NewStore(cache.MetaNamespaceKeyFunc, indexers, testGenEvent)

		testCase.operations(store)
		objs, err := store.GetByIndex(indexName, indexKey)
		if err != nil {
			t.Errorf("%d: failed to get object by index: %v", i, err)
		}
		if !reflect.DeepEqual(objs, testCase.expected) {
			t.Errorf("%d: get unexpected object: %v", i, objs)
		}
	}
}

func TestRamStoreWatchAll(t *testing.T) {
	testCases := []struct {
		// The operations that will be executed on the storage
		operations func(*store)
		// The events expected to see
		expected []watch.Event
	}{
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}})
				store.Update(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx2"}}})
			},
			expected: []watch.Event{
				{watch.Added, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}}},
				{watch.Modified, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx2"}}}},
			},
		},
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}})
				store.Delete("pod1")
			},
			expected: []watch.Event{
				{watch.Added, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}}},
				{watch.Deleted, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}}},
			},
		},
	}
	for i, testCase := range testCases {
		store := NewStore(cache.MetaNamespaceKeyFunc, cache.Indexers{}, testGenEvent)
		w, err := store.Watch(context.Background(), "", labels.Everything(), fields.Everything())
		if err != nil {
			t.Errorf("%d: failed to watch object: %v", i, err)
		}
		testCase.operations(store)
		ch := w.ResultChan()
		for j, expectedEvent := range testCase.expected {
			actualEvent := <-ch
			if !reflect.DeepEqual(actualEvent, expectedEvent) {
				t.Errorf("%d: unexpected event %d", i, j)
			}
		}
		select {
		case obj, ok := <-ch:
			t.Errorf("%d: unexpected excess event: %#v %t", i, obj, ok)
		default:
		}
	}
}

func TestRamStoreWatchWithInitOperations(t *testing.T) {
	testCases := []struct {
		// The operations that will be executed on the storage before watching
		initOperations func(*store)
		// The operations that will be executed on the storage after watching
		operations func(*store)
		// We should see the initOperations merged and watched as "ADDED" events
		// before the events generated by operations
		expected []watch.Event
	}{
		{
			initOperations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}})
				store.Update(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx2"}}})
				store.Update(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx3"}}})
			},
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"app": "nginx2"}}})
				store.Update(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"app": "nginx3"}}})
			},
			expected: []watch.Event{
				{watch.Added, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx3"}}}},
				{watch.Added, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"app": "nginx2"}}}},
				{watch.Modified, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"app": "nginx3"}}}},
			},
		},
		{
			initOperations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}})
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"app": "nginx2"}}})
				store.Delete("pod2")
			},
			operations: func(store *store) {
				store.Delete("pod1")
			},
			expected: []watch.Event{
				{watch.Added, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}}},
				{watch.Deleted, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}}},
			},
		},
	}
	for i, testCase := range testCases {
		store := NewStore(cache.MetaNamespaceKeyFunc, cache.Indexers{}, testGenEvent)
		// Init the storage before watching
		testCase.initOperations(store)
		w, err := store.Watch(context.Background(), "", labels.Everything(), fields.Everything())
		if err != nil {
			t.Errorf("%d: failed to watch object: %v", i, err)
		}
		testCase.operations(store)
		ch := w.ResultChan()
		for j, expectedEvent := range testCase.expected {
			actualEvent := <-ch
			if !reflect.DeepEqual(actualEvent, expectedEvent) {
				t.Errorf("%d: unexpected event %d", i, j)
			}
		}
		select {
		case obj, ok := <-ch:
			t.Errorf("%d: unexpected excess event: %#v %t", i, obj, ok)
		default:
		}
	}
}

func TestRamStoreWatchWithSelector(t *testing.T) {
	testCases := []struct {
		// The operations that will be executed on the storage before watching
		operations func(*store)
		// The label Selector that will be set when watching
		labelSelector labels.Selector
		// The events expected to see, there should be only events matching the labelSelector
		expected []watch.Event
	}{
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}})
				store.Update(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx2"}}})
			},
			labelSelector: labels.SelectorFromSet(labels.Set{"app": "nginx1"}),
			expected: []watch.Event{
				{watch.Added, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}}},
				{watch.Deleted, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}}},
			},
		},
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}})
				store.Update(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx2"}}})
			},
			labelSelector: labels.SelectorFromSet(labels.Set{"app": "nginx2"}),
			expected: []watch.Event{
				{watch.Added, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx2"}}}},
			},
		},
		{
			operations: func(store *store) {
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}})
				store.Create(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{"app": "nginx2"}}})
				store.Delete("pod1")
				store.Delete("pod2")
			},
			labelSelector: labels.SelectorFromSet(labels.Set{"app": "nginx1"}),
			expected: []watch.Event{
				{watch.Added, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}}},
				{watch.Deleted, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{"app": "nginx1"}}}},
			},
		},
	}
	for i, testCase := range testCases {
		store := NewStore(cache.MetaNamespaceKeyFunc, cache.Indexers{}, testGenEvent)
		w, err := store.Watch(context.Background(), "", testCase.labelSelector, fields.Everything())
		if err != nil {
			t.Errorf("%d: failed to watch object: %v", i, err)
		}
		testCase.operations(store)
		ch := w.ResultChan()
		for j, expectedEvent := range testCase.expected {
			actualEvent := <-ch
			if !reflect.DeepEqual(actualEvent, expectedEvent) {
				t.Errorf("%d: unexpected event %d", i, j)
			}
		}
		select {
		case obj, ok := <-ch:
			t.Errorf("%d: unexpected excess event: %#v %t", i, obj, ok)
		default:
		}
	}
}
