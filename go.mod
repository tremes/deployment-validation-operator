module github.com/app-sre/deployment-validation-operator

go 1.16

require (
	github.com/ghodss/yaml v1.0.1-0.20190212211648-25d852aebe32
	github.com/go-openapi/swag v0.19.15 // indirect
	github.com/mcuadros/go-defaults v1.2.0
	github.com/openshift/client-go v3.9.0+incompatible
	github.com/operator-framework/operator-lib v0.4.1
	github.com/prometheus/client_golang v1.12.1
	github.com/spf13/pflag v1.0.5
	github.com/spf13/viper v1.11.0
	golang.stackrox.io/kube-linter v0.0.0-20220512143000-bf3a4dbd967c
	k8s.io/api v0.24.0
	k8s.io/apimachinery v0.24.0
	k8s.io/client-go v0.24.0
	sigs.k8s.io/controller-runtime v0.12.1
)

replace (
	github.com/Azure/go-autorest => github.com/Azure/go-autorest v14.2.0+incompatible // Required by OLM
	github.com/go-openapi/spec => github.com/go-openapi/spec v0.0.0-20180415031709-bcff419492ee
	github.com/operator-framework/operator-sdk => github.com/operator-framework/operator-sdk v1.7.2
	github.com/prometheus-operator/prometheus-operator => github.com/prometheus-operator/prometheus-operator v0.46.0
)

exclude github.com/spf13/viper v1.3.2 // Required to fix CVE-2018-1098
