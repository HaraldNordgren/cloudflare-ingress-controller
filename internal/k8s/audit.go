package k8s

import (
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// EndpointsHaveSubsets verify that subsets exist
func EndpointsHaveSubsets(ep *v1.Endpoints) bool {
	if ep != nil {
		for _, subset := range ep.Subsets {
			if len(subset.Addresses) > 0 {
				return true
			}
		}
	}
	return false
}

// GetServicePort checks for a port defined by a service
func GetServicePort(svc *v1.Service, port intstr.IntOrString) (val int32, exists bool) {
	if svc != nil {
		for _, servicePort := range svc.Spec.Ports {
			switch port.Type {
			case intstr.Int:
				if servicePort.Port == port.IntVal {
					return servicePort.Port, true
				}
			case intstr.String:
				if servicePort.Name == port.StrVal {
					return servicePort.Port, true
				}
			}
		}
	}
	return
}
