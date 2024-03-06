package utils

import (
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func GetLabels(object client.Object) labels.Set {
	return labels.Set(object.GetLabels())
}
