package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/cache"
	managerCache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type configMapWatcher struct {
	cli client.Client
	ca  managerCache.Cache
}

func NewConfigMapWatcher(m manager.Manager) configMapWatcher {

	return configMapWatcher{
		cli: m.GetClient(),
		ca:  m.GetCache(),
	}
}

func (c configMapWatcher) Start(ctx context.Context) error {
	cmKey := client.ObjectKey{
		Name:      "deployment-validation-operator-config",
		Namespace: "deployment-validation-operator",
	}

	var configMap corev1.ConfigMap
	err := c.cli.Get(ctx, cmKey, &configMap)
	if err != nil {
		return err
	}
	inf, err := c.ca.GetInformer(ctx, &configMap)

	if err != nil {
		return err
	}

	inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {},
		UpdateFunc: func(oldObj, newObj interface{}) {
			obj, err := meta.Accessor(newObj)
			if err != nil {
				fmt.Println("============ ERROR ", err)
			}
			fmt.Println("===================== UPDATE CALLED ", obj.GetName())
			if obj.GetName() == "deployment-validation-operator-config" {
				fmt.Println("===================== OLD ", oldObj)
				fmt.Println("===================== NEW ", newObj)
			}
		},
	})
	return nil
}
