/*
Copyright 2021.

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

package controller

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/app-sre/deployment-validation-operator/pkg/utils"
	"github.com/app-sre/deployment-validation-operator/pkg/validations"
	"github.com/go-logr/logr"
	v1apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	v1policy "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ValidationController watches all the registered
// resource Kinds and runs the Kubelinter validation for them
type ValidationController struct {
	client.Client
	Log                   logr.Logger
	Scheme                *runtime.Scheme
	namespaceIgnoreRegex  regexp.Regexp
	resourceGVKs          []schema.GroupVersionKind
	validationEngine      validations.Interface
	objectValidationCache *validationCache
	currentObjects        *validationCache
}

func NewController(cli client.Client, log logr.Logger, validationEngine validations.Interface) (*ValidationController, error) {
	reconciler := &ValidationController{
		Client:           cli,
		Log:              log,
		validationEngine: validationEngine,
		resourceGVKs: []schema.GroupVersionKind{
			schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "Pod",
			},
			schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "Service",
			},
			schema.GroupVersionKind{
				Group:   "apps",
				Version: "v1",
				Kind:    "Deployment",
			},
			schema.GroupVersionKind{
				Group:   "policy",
				Version: "v1",
				Kind:    "PodDisruptionBudget",
			},
		},
		objectValidationCache: newValidationCache(),
		currentObjects:        newValidationCache(),
	}

	ignorePatternStr := os.Getenv(EnvNamespaceIgnorePattern)
	if ignorePatternStr != "" {
		nsIgnoreRegex, err := regexp.Compile(ignorePatternStr)
		if err != nil {
			return nil, err
		}
		reconciler.namespaceIgnoreRegex = *nsIgnoreRegex
	}

	return reconciler, nil
}

func (v *ValidationController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := v.Log.WithValues("validation-controller", req.NamespacedName)

	nsUID, err := v.getNamespaceUID(ctx, req.Namespace)
	if err != nil {
		log.Error(err, "failed to get UID of the namespace", req.Namespace)
		return ctrl.Result{
			RequeueAfter: 5 * time.Second,
		}, nil
	}

	log.Info("reconcile", "Validating objects in the namespace", req.Namespace)
	relatedObjects := v.groupObjects(ctx, req.Namespace)

	for label, objects := range relatedObjects {
		err := v.validateObjects(label, objects, req.Namespace, nsUID)
		if err != nil {
			return ctrl.Result{
				RequeueAfter: 5 * time.Second,
			}, nil
		}
	}

	v.handleResourceDeletions(nsUID)
	return ctrl.Result{}, nil
}

// validateObjects
func (v *ValidationController) validateObjects(label string, objects []*unstructured.Unstructured, namespace, namespaceUID string) error {
	var typedObjects []client.Object
	for _, o := range objects {
		typedObj, err := v.unstructuredToTyped(o)
		if err != nil {
			return err
		}
		typedObjects = append(typedObjects, typedObj)
	}

	if v.allObjectsValidated(typedObjects, namespaceUID) {
		v.Log.Info("validateObjects", "All objects are validated", "Nothing to do")
		return nil
	}

	v.Log.Info("validateObjects", "Validating objects satisfying label", label, "in the namespace", namespace)
	validationOutcome, err := v.validationEngine.RunValidationsForObjects(typedObjects, namespaceUID)
	if err != nil {
		return err
	}
	for _, o := range objects {
		v.objectValidationCache.store(o, namespaceUID, validationOutcome)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (v *ValidationController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("ValidationController").
		Watches(&v1policy.PodDisruptionBudget{}, &handler.EnqueueRequestForObject{}).
		Watches(&v1apps.Deployment{}, &handler.EnqueueRequestForObject{}).
		Watches(&v1.Pod{}, &handler.EnqueueRequestForObject{}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(ue event.UpdateEvent) bool {
				if v.isNamespaceExcluded(ue.ObjectNew.GetNamespace()) {
					return false
				}
				oldGeneration := ue.ObjectOld.GetGeneration()
				newGeneration := ue.ObjectNew.GetGeneration()

				return oldGeneration != newGeneration
			},
			DeleteFunc: func(de event.DeleteEvent) bool {
				return !v.isNamespaceExcluded(de.Object.GetNamespace())
			},
			CreateFunc: func(ce event.CreateEvent) bool {
				return !v.isNamespaceExcluded(ce.Object.GetNamespace())
			},
		}).Complete(v)
}

func (v *ValidationController) isNamespaceExcluded(namespace string) bool {
	return v.namespaceIgnoreRegex.Match([]byte(namespace))
}

func (v *ValidationController) getNamespaceUID(ctx context.Context, namespace string) (string, error) {
	key := client.ObjectKey{
		Name:      namespace,
		Namespace: namespace,
	}

	var ns v1.Namespace
	err := v.Client.Get(ctx, key, &ns)
	if err != nil {
		return "", err
	}
	return string(ns.UID), nil
}

// groupAppObjects iterates over provided GroupVersionKind in given namespace
// and returns map of objects grouped by their "app" label
func (v *ValidationController) groupObjects(ctx context.Context, namespace string) map[string][]*unstructured.Unstructured {
	relatedObjects := make(map[string][]*unstructured.Unstructured)

	for _, gvk := range v.resourceGVKs {
		list := unstructured.UnstructuredList{}
		listOptions := &client.ListOptions{
			//Limit:     gr.listLimit,
			Namespace: namespace,
		}
		list.SetGroupVersionKind(gvk)

		if err := v.Client.List(ctx, &list, listOptions); err != nil {
			continue
		}

		for i := range list.Items {
			obj := &list.Items[i]
			unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
			unstructured.RemoveNestedField(obj.Object, "status")
			processResourceLabels(obj, relatedObjects)
			v.processResourceSelectors(obj, relatedObjects)
		}
	}
	return relatedObjects
}

// processResourceSelectors reads resource selector and then tries to match
// the selector to known labels (keys in the relatedObjects map). If a match is found then
// the object is added to the corresponding group (values in the relatedObjects map).
func (v *ValidationController) processResourceSelectors(obj *unstructured.Unstructured,
	relatedObjects map[string][]*unstructured.Unstructured) {
	labelSelector := utils.GetLabelSelector(obj)
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		v.Log.Error(err, "cannot convert label selector for object", obj.GetKind(), obj.GetName())
		return
	}

	if selector == labels.Nothing() {
		return
	}

	for k := range relatedObjects {
		labelsSet, err := labels.ConvertSelectorToLabelsMap(k)
		if err != nil {
			v.Log.Error(err, "cannot convert selector to labels map for", obj.GetKind(), obj.GetName())
			continue
		}
		if selector.Matches(labelsSet) {
			relatedObjects[k] = append(relatedObjects[k], obj)
		}
	}
}

// processResourceLabels reads resource labels and if the labels
// are not empty then format them into string and put the string value
// as key and the object as a value into "relatedObjects" map
func processResourceLabels(obj *unstructured.Unstructured,
	relatedObjects map[string][]*unstructured.Unstructured) {

	objLabels := utils.GetLabels(obj)
	if len(objLabels) == 0 {
		return
	}
	labelsString := labels.FormatLabels(objLabels)
	relatedObjects[labelsString] = append(relatedObjects[labelsString], obj)
}

// allObjectsValidated checks whether all unstructured objects passed as argument are validated
// and thus present in the cache
func (g *ValidationController) allObjectsValidated(objs []client.Object, namespaceUID string) bool {
	allObjectsValidated := true
	// we must be sure that all objects in the given group are cached (validated)
	// see DVO-103
	for _, o := range objs {
		g.currentObjects.store(o, namespaceUID, "")
		if !g.objectValidationCache.objectAlreadyValidated(o, namespaceUID) {
			allObjectsValidated = false
		}
	}
	return allObjectsValidated
}

func (g *ValidationController) unstructuredToTyped(obj *unstructured.Unstructured) (client.Object, error) {
	typedResource, err := g.lookUpType(obj)
	if err != nil {
		return nil, fmt.Errorf("looking up object type: %w", err)
	}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, typedResource); err != nil {
		return nil, fmt.Errorf("converting unstructured to typed object: %w", err)
	}

	return typedResource.(client.Object), nil
}
func (g *ValidationController) lookUpType(obj *unstructured.Unstructured) (runtime.Object, error) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	typedObj, err := g.Client.Scheme().New(gvk)
	if err != nil {
		return nil, fmt.Errorf("creating new object of type %s: %w", gvk, err)
	}

	return typedObj, nil
}

func (gr *ValidationController) handleResourceDeletions(namespaceUID string) {
	for k, v := range *gr.objectValidationCache {
		if gr.currentObjects.has(k) {
			continue
		}

		req := validations.Request{
			Kind:         k.kind,
			Name:         k.name,
			Namespace:    k.namespace,
			NamespaceUID: namespaceUID,
			UID:          v.uid,
		}

		gr.validationEngine.DeleteMetrics(req.ToPromLabels())

		gr.objectValidationCache.removeKey(k)

	}
	gr.currentObjects.drain()
}
