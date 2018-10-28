package argotunnel

import (
	"fmt"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type informerset struct {
	endpoint cache.SharedIndexInformer
	ingress  cache.SharedIndexInformer
	secret   cache.SharedIndexInformer
	service  cache.SharedIndexInformer
}

func (i *informerset) run(stopCh <-chan struct{}) {
	go i.endpoint.Run(stopCh)
	go i.ingress.Run(stopCh)
	go i.secret.Run(stopCh)
	go i.service.Run(stopCh)
}

func (i *informerset) waitForCacheSync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh,
		i.endpoint.HasSynced,
		i.ingress.HasSynced,
		i.secret.HasSynced,
		i.service.HasSynced,
	)
}

func newEndpointInformer(client kubernetes.Interface, opts options, rs ...cache.ResourceEventHandler) cache.SharedIndexInformer {
	return newInformer(client.CoreV1().RESTClient(), "endpoints", new(v1.Endpoints), opts.resyncPeriod, rs...)
}

// TODO: customize the ingress informer/indexer manage route objects internally
func newIngressInformer(client kubernetes.Interface, opts options, rs ...cache.ResourceEventHandler) cache.SharedIndexInformer {
	i := newInformer(client.ExtensionsV1beta1().RESTClient(), "ingresses", new(v1beta1.Ingress), opts.resyncPeriod, rs...)
	i.AddIndexers(cache.Indexers{
		secretKind:  ingressSecretIndexFunc(opts.secret),
		serviceKind: ingressServiceIndexFunc(),
	})
	return i
}

func newSecretInformer(client kubernetes.Interface, opts options, rs ...cache.ResourceEventHandler) cache.SharedIndexInformer {
	return newInformer(client.CoreV1().RESTClient(), "secrets", new(v1.Secret), opts.resyncPeriod, rs...)
}

func newServiceInformer(client kubernetes.Interface, opts options, rs ...cache.ResourceEventHandler) cache.SharedIndexInformer {
	return newInformer(client.CoreV1().RESTClient(), "services", new(v1.Service), opts.resyncPeriod, rs...)
}

func newInformer(c cache.Getter, resource string, objType runtime.Object, resyncPeriod time.Duration, rs ...cache.ResourceEventHandler) cache.SharedIndexInformer {
	lw := cache.NewListWatchFromClient(c, resource, v1.NamespaceAll, fields.Everything())
	sw := cache.NewSharedIndexInformer(lw, objType, resyncPeriod, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	for _, r := range rs {
		sw.AddEventHandler(r)
	}
	return sw
}

func ingressSecretIndexFunc(secret *resource) func(obj interface{}) ([]string, error) {
	return func(obj interface{}) ([]string, error) {
		if ing, ok := obj.(*v1beta1.Ingress); ok {
			hostsecret := make(map[string]*resource)
			for _, tls := range ing.Spec.TLS {
				for _, host := range tls.Hosts {
					if len(tls.SecretName) > 0 {
						hostsecret[host] = &resource{
							name:      tls.SecretName,
							namespace: ing.Namespace,
						}
					}
				}
			}
			var idx []string
			for _, rule := range ing.Spec.Rules {
				if rule.HTTP != nil && len(rule.Host) > 0 {
					if r, ok := hostsecret[rule.Host]; ok {
						idx = append(idx, itemKeyFunc(r.namespace, r.name))
					} else if secret != nil {
						idx = append(idx, itemKeyFunc(secret.namespace, secret.name))
					}
				}
			}
			return idx, nil
		}
		return []string{}, fmt.Errorf("index unexpected obj type: %T", obj)
	}
}

func ingressServiceIndexFunc() func(obj interface{}) ([]string, error) {
	return func(obj interface{}) ([]string, error) {
		if ing, ok := obj.(*v1beta1.Ingress); ok {
			var idx []string
			for _, rule := range ing.Spec.Rules {
				if rule.HTTP != nil && len(rule.Host) > 0 {
					for _, path := range rule.HTTP.Paths {
						if len(path.Backend.ServiceName) > 0 {
							idx = append(idx, itemKeyFunc(ing.Namespace, path.Backend.ServiceName))
						}
					}
				}
			}
			return idx, nil
		}
		return []string{}, fmt.Errorf("index unexpected obj type: %T", obj)
	}
}

func itemKeyFunc(namespace, name string) (key string) {
	key = namespace + "/" + name
	return
}
