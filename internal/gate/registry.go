package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type subscription struct {
	source  GateSource
	matcher Matcher
	specKey string
}
type observer struct {
	signature     string
	subscriptions []subscription
}
type subject struct {
	informer cache.SharedIndexInformer
	cancel   context.CancelFunc
	// namespace/name (empty name denotes a selector) -> observers
	index map[types.NamespacedName]map[types.NamespacedName][]Matcher
}

// Registry is both the shared informer owner and the controller's event source.
// Informer callbacks only add keys to the controller workqueue; all evaluation
// happens in level-triggered reconciliation against synchronized caches.
type Registry struct {
	mu        sync.RWMutex
	dynamic   dynamic.Interface
	allowed   map[schema.GroupVersionKind]schema.GroupVersionResource
	subjects  map[schema.GroupVersionKind]*subject
	observers map[types.NamespacedName]observer
	ctx       context.Context
	queue     workqueue.TypedRateLimitingInterface[reconcile.Request]
}

func NewRegistry(client dynamic.Interface, allowed []Kind) (*Registry, error) {
	r := &Registry{dynamic: client, allowed: map[schema.GroupVersionKind]schema.GroupVersionResource{}, subjects: map[schema.GroupVersionKind]*subject{}, observers: map[types.NamespacedName]observer{}}
	for _, kind := range allowed {
		if strings.ContainsAny(kind.APIVersion+kind.Kind+kind.Resource, "*'\"\\") {
			return nil, fmt.Errorf("gate allowlist must contain exact GVK/resources")
		}
		gv, err := schema.ParseGroupVersion(kind.APIVersion)
		if err != nil || kind.APIVersion == "" || kind.Kind == "" || kind.Resource == "" {
			return nil, fmt.Errorf("invalid gate kind allowlist entry: %+v", kind)
		}
		r.allowed[gv.WithKind(kind.Kind)] = gv.WithResource(kind.Resource)
	}
	return r, nil
}

func (r *Registry) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctx, r.queue = ctx, queue
	return nil
}

func (r *Registry) Subscribe(key types.NamespacedName, generation int64, specs []v1.GateSpec) error {
	encoded, _ := json.Marshal(struct {
		Generation int64
		Specs      []v1.GateSpec
	}{generation, specs})
	signature := string(encoded)
	r.mu.Lock()
	defer r.mu.Unlock()
	if previous, ok := r.observers[key]; ok && previous.signature == signature {
		return nil
	}
	var subscriptions []subscription
	for _, spec := range specs {
		source, err := Compile(spec)
		if err != nil {
			return err
		}
		if _, ok := r.allowed[source.GVK()]; !ok {
			return fmt.Errorf("gate GVK %s is not allowlisted", source.GVK())
		}
		matcher, err := source.Match(spec)
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(spec)
		subscriptions = append(subscriptions, subscription{source, matcher, string(encoded)})
		// workloadRef Deployments change without changing the Rollout generation.
		// Subscribe to their namespace too; Evaluate checks the exact reference.
		if spec.Preset == "argo-rollout" {
			dep, _ := Compile(v1.GateSpec{Preset: "deployment", Namespace: spec.Namespace, Name: "dependency"})
			if _, ok := r.allowed[dep.GVK()]; !ok {
				return fmt.Errorf("argo-rollout gate also requires allowlisted apps/v1 Deployment")
			}
			subscriptions = append(subscriptions, subscription{source: dep, matcher: Matcher{Namespace: spec.Namespace, Selector: labels.Everything()}})
		}
	}
	// Detach only after the new generation validates and compiles successfully.
	// Preserve shared subjects while changing matchers to avoid cache churn.
	r.detachLocked(key, false)
	for _, sub := range subscriptions {
		gvk := sub.source.GVK()
		s := r.subjects[gvk]
		if s == nil {
			if r.ctx == nil {
				return fmt.Errorf("gate registry has not started")
			}
			ctx, cancel := context.WithCancel(r.ctx)
			informer := dynamicinformer.NewFilteredDynamicInformer(r.dynamic, r.allowed[gvk], "", 30*time.Minute, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, nil).Informer()
			s = &subject{informer: informer, cancel: cancel, index: map[types.NamespacedName]map[types.NamespacedName][]Matcher{}}
			r.subjects[gvk] = s
			_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
				AddFunc:    func(obj interface{}) { r.notify(gvk, obj) },
				UpdateFunc: func(old, current interface{}) { r.notify(gvk, old); r.notify(gvk, current) },
				DeleteFunc: func(obj interface{}) { r.notify(gvk, obj) },
			})
			if err != nil {
				cancel()
				return err
			}
			go informer.Run(ctx.Done())
		}
		matchKey := types.NamespacedName{Namespace: sub.matcher.Namespace, Name: sub.matcher.Name}
		if s.index[matchKey] == nil {
			s.index[matchKey] = map[types.NamespacedName][]Matcher{}
		}
		s.index[matchKey][key] = append(s.index[matchKey][key], sub.matcher)
	}
	r.observers[key] = observer{signature, subscriptions}
	r.pruneLocked()
	return nil
}

func (r *Registry) Unsubscribe(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detachLocked(key, true)
}
func (r *Registry) detachLocked(key types.NamespacedName, prune bool) {
	for _, s := range r.subjects {
		for indexKey, owners := range s.index {
			delete(owners, key)
			if len(owners) == 0 {
				delete(s.index, indexKey)
			}
		}
	}
	delete(r.observers, key)
	if prune {
		r.pruneLocked()
	}
}
func (r *Registry) pruneLocked() {
	for gvk, s := range r.subjects {
		if len(s.index) == 0 {
			s.cancel()
			delete(r.subjects, gvk)
		}
	}
}
func (r *Registry) notify(gvk schema.GroupVersionKind, raw interface{}) {
	if tombstone, ok := raw.(cache.DeletedFinalStateUnknown); ok {
		raw = tombstone.Obj
	}
	obj, ok := raw.(*unstructured.Unstructured)
	if !ok {
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.subjects[gvk]
	if s == nil || r.queue == nil {
		return
	}
	for _, matchKey := range []types.NamespacedName{{Namespace: obj.GetNamespace(), Name: obj.GetName()}, {Namespace: obj.GetNamespace()}} {
		for owner, matchers := range s.index[matchKey] {
			for _, matcher := range matchers {
				if matcher.Matches(obj) {
					r.queue.Add(reconcile.Request{NamespacedName: owner})
					break
				}
			}
		}
	}
}

func (r *Registry) Object(gvk schema.GroupVersionKind, key types.NamespacedName) (*unstructured.Unstructured, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.subjects[gvk]
	if s == nil || !s.informer.HasSynced() {
		return nil, fmt.Errorf("gate cache %s is not synchronized", gvk)
	}
	obj, found, err := s.informer.GetStore().GetByKey(key.String())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("gate object %s %s is absent", gvk, key)
	}
	return obj.(*unstructured.Unstructured).DeepCopy(), nil
}

func (r *Registry) Synced(key types.NamespacedName) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	owner, ok := r.observers[key]
	if !ok {
		return false
	}
	for _, sub := range owner.subscriptions {
		if s := r.subjects[sub.source.GVK()]; s == nil || !s.informer.HasSynced() {
			return false
		}
	}
	return true
}

func (r *Registry) compiled(spec v1.GateSpec) GateSource {
	encoded, _ := json.Marshal(spec)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, owner := range r.observers {
		for _, sub := range owner.subscriptions {
			if sub.specKey == string(encoded) {
				return sub.source
			}
		}
	}
	return nil
}

// Evaluate returns no success for an empty selector or an unsynchronized cache.
// Every selected object must deploy and become healthy; only a deployed object
// can contribute failure. The caller combines these with Git observation.
func (r *Registry) Evaluate(specs []v1.GateSpec, tag string) (deployed, healthy, failed bool, reason string) {
	deployed, healthy = true, true
	for _, spec := range specs {
		source := r.compiled(spec)
		if source == nil {
			return false, false, false, "gate is not subscribed"
		}
		matcher, _ := source.Match(spec)
		r.mu.RLock()
		s := r.subjects[source.GVK()]
		if s == nil || !s.informer.HasSynced() {
			r.mu.RUnlock()
			return false, false, false, "gate cache not synchronized"
		}
		items, err := s.informer.GetIndexer().ByIndex(cache.NamespaceIndex, matcher.Namespace)
		r.mu.RUnlock()
		if err != nil {
			return false, false, false, err.Error()
		}
		matched := 0
		for _, item := range items {
			obj := item.(*unstructured.Unstructured).DeepCopy()
			if !matcher.Matches(obj) {
				continue
			}
			matched++
			if spec.Preset == "argo-rollout" && text(obj, "spec", "workloadRef", "name") != "" {
				if text(obj, "spec", "workloadRef", "apiVersion") != "apps/v1" || text(obj, "spec", "workloadRef", "kind") != "Deployment" {
					return false, false, false, "unsupported workloadRef"
				}
				dep, err := r.Object(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, types.NamespacedName{Namespace: obj.GetNamespace(), Name: text(obj, "spec", "workloadRef", "name")})
				if err != nil {
					return false, false, false, err.Error()
				}
				if text(obj, "status", "workloadObservedGeneration") != fmt.Sprint(dep.GetGeneration()) {
					deployed, healthy = false, false
					continue
				}
				template, _, _ := unstructured.NestedMap(dep.Object, "spec", "template")
				_ = unstructured.SetNestedMap(obj.Object, template, "spec", "template")
			}
			d, h, f, why := source.Evaluate(obj, tag)
			deployed, healthy, failed = deployed && d, healthy && h, failed || (d && f)
			if !h {
				reason = why
			}
		}
		if matched == 0 {
			deployed, healthy, reason = false, false, "no matching gate objects"
		}
	}
	return
}
