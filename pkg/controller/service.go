package controller

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/containous/maesh/pkg/annotations"
	"github.com/containous/maesh/pkg/k8s"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
)

// ShadowServiceManager manages shadow services.
type ShadowServiceManager struct {
	log                logrus.FieldLogger
	lister             listers.ServiceLister
	namespace          string
	tcpStateTable      PortMapper
	udpStateTable      PortMapper
	defaultTrafficType string
	minHTTPPort        int32
	maxHTTPPort        int32
	kubeClient         kubernetes.Interface
}

// NewShadowServiceManager returns new shadow service manager.
func NewShadowServiceManager(log logrus.FieldLogger, lister listers.ServiceLister, namespace string, tcpStateTable PortMapper, udpStateTable PortMapper, defaultTrafficType string, minHTTPPort, maxHTTPPort int32, kubeClient kubernetes.Interface) *ShadowServiceManager {
	return &ShadowServiceManager{
		log:                log,
		lister:             lister,
		namespace:          namespace,
		tcpStateTable:      tcpStateTable,
		udpStateTable:      udpStateTable,
		defaultTrafficType: defaultTrafficType,
		minHTTPPort:        minHTTPPort,
		maxHTTPPort:        maxHTTPPort,
		kubeClient:         kubeClient,
	}
}

// Create creates a new shadow service based on the given service.
func (s *ShadowServiceManager) Create(userSvc *corev1.Service) error {
	name := s.getShadowServiceName(userSvc.Name, userSvc.Namespace)

	s.log.Debugf("Creating mesh service: %s", name)

	_, err := s.lister.Services(s.namespace).Get(name)
	if err == nil {
		return nil
	}

	if !kerrors.IsNotFound(err) {
		return fmt.Errorf("unable to get shadow service %q: %w", name, err)
	}

	ports, err := s.getShadowServicePorts(userSvc)
	if err != nil {
		return fmt.Errorf("unable to get ports for service %s/%s: %w", userSvc.Namespace, userSvc.Name, err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels: map[string]string{
				"app":  "maesh",
				"type": "shadow",
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: ports,
			Selector: map[string]string{
				"component": "maesh-mesh",
			},
		},
	}

	major, minor := parseKubernetesServerVersion(s.kubeClient)

	// If the kubernetes server version is 1.17+, then use the topology key.
	if major == 1 && minor >= 17 {
		svc.Spec.TopologyKeys = []string{
			"kubernetes.io/hostname",
			"topology.kubernetes.io/zone",
			"topology.kubernetes.io/region",
			"*",
		}
	}

	if _, err = s.kubeClient.CoreV1().Services(s.namespace).Create(svc); err != nil {
		return fmt.Errorf("unable to create kubernetes service: %w", err)
	}

	return nil
}

// Update updates the shadow service associated with the old user service following the content of the new user service.
func (s *ShadowServiceManager) Update(oldUserSvc *corev1.Service, newUserSvc *corev1.Service) (*corev1.Service, error) {
	name := s.getShadowServiceName(newUserSvc.Name, newUserSvc.Namespace)

	if err := s.cleanupPortMapping(oldUserSvc, newUserSvc); err != nil {
		return nil, fmt.Errorf("unable to cleanup port mapping for service %s/%s: %w", oldUserSvc.Namespace, oldUserSvc.Name, err)
	}

	ports, err := s.getShadowServicePorts(newUserSvc)
	if err != nil {
		return nil, fmt.Errorf("unable to get ports for service %s/%s: %w", newUserSvc.Namespace, newUserSvc.Name, err)
	}

	var updatedSvc *corev1.Service

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		svc, err := s.lister.Services(s.namespace).Get(name)
		if err != nil {
			return fmt.Errorf("unable to get shadow service %q: %w", name, err)
		}

		newSvc := svc.DeepCopy()
		newSvc.Spec.Ports = ports

		if updatedSvc, err = s.kubeClient.CoreV1().Services(s.namespace).Update(newSvc); err != nil {
			return fmt.Errorf("unable to update kubernetes service: %w", err)
		}

		return nil
	})

	if retryErr != nil {
		return nil, fmt.Errorf("unable to update service %q: %v", name, retryErr)
	}

	s.log.Debugf("Updated service: %s/%s", s.namespace, name)

	return updatedSvc, nil
}

// Delete deletes a shadow service associated with the given user service.
func (s *ShadowServiceManager) Delete(userSvc *corev1.Service) error {
	name := s.getShadowServiceName(userSvc.Name, userSvc.Namespace)

	if err := s.cleanupPortMapping(userSvc, nil); err != nil {
		return fmt.Errorf("unable to cleanup port mapping for service %s/%s: %w", userSvc.Namespace, userSvc.Name, err)
	}

	_, err := s.lister.Services(s.namespace).Get(name)
	if err != nil {
		return err
	}

	if err := s.kubeClient.CoreV1().Services(s.namespace).Delete(name, &metav1.DeleteOptions{}); err != nil {
		return err
	}

	s.log.Debugf("Deleted service: %s/%s", s.namespace, name)

	return nil
}

func (s *ShadowServiceManager) cleanupPortMapping(oldUserSvc *corev1.Service, newUserSvc *corev1.Service) error {
	var stateTable PortMapper

	trafficType, err := annotations.GetTrafficType(s.defaultTrafficType, oldUserSvc.Annotations)
	if err != nil {
		return fmt.Errorf("unable to get service traffic type: %w", err)
	}

	switch trafficType {
	case annotations.ServiceTypeTCP:
		stateTable = s.tcpStateTable
	case annotations.ServiceTypeUDP:
		stateTable = s.udpStateTable
	default:
		return nil
	}

	for _, old := range oldUserSvc.Spec.Ports {
		var found bool

		if newUserSvc != nil {
			for _, new := range newUserSvc.Spec.Ports {
				if old.Port == new.Port {
					found = true
					break
				}
			}
		}

		if !found {
			_, err := stateTable.Remove(k8s.ServiceWithPort{
				Namespace: oldUserSvc.Namespace,
				Name:      oldUserSvc.Name,
				Port:      old.Port,
			})

			if err != nil {
				s.log.Warnf("Unable to remove port mapping for %s/%s on port %d", oldUserSvc.Namespace, oldUserSvc.Name, old.Port)
			}
		}
	}

	return nil
}

func (s *ShadowServiceManager) getShadowServicePorts(svc *corev1.Service) ([]corev1.ServicePort, error) {
	var ports []corev1.ServicePort

	trafficType, err := annotations.GetTrafficType(s.defaultTrafficType, svc.Annotations)
	if err != nil {
		return nil, fmt.Errorf("unable to get service traffic-type: %w", err)
	}

	for i, sp := range svc.Spec.Ports {
		if !isPortSuitable(trafficType, sp) {
			s.log.Warnf("Unsupported port type %q on %q service %s/%s, skipping port %q", sp.Protocol, trafficType, svc.Namespace, svc.Name, sp.Name)
		}

		targetPort, err := s.getTargetPort(trafficType, i, svc.Name, svc.Namespace, sp.Port)
		if err != nil {
			s.log.Errorf("Unable to find available %s port: %v, skipping port %s on service %s/%s", sp.Name, err, sp.Name, svc.Namespace, svc.Name)
			continue
		}

		ports = append(ports, corev1.ServicePort{
			Name:       sp.Name,
			Port:       sp.Port,
			Protocol:   sp.Protocol,
			TargetPort: intstr.FromInt(int(targetPort)),
		})
	}

	return ports, nil
}

// getShadowServiceName converts a User service with a namespace to a mesh service name.
func (s *ShadowServiceManager) getShadowServiceName(name string, namespace string) string {
	return fmt.Sprintf("%s-%s-6d61657368-%s", s.namespace, name, namespace)
}

func (s *ShadowServiceManager) getTargetPort(trafficType string, portID int, name, namespace string, port int32) (int32, error) {
	switch trafficType {
	case annotations.ServiceTypeHTTP:
		return s.getHTTPPort(portID)
	case annotations.ServiceTypeTCP:
		return s.getMappedPort(s.tcpStateTable, name, namespace, port)
	case annotations.ServiceTypeUDP:
		return s.getMappedPort(s.udpStateTable, name, namespace, port)
	default:
		return 0, errors.New("unknown service mode")
	}
}

// getHTTPPort returns the HTTP port associated with the given portID.
func (s *ShadowServiceManager) getHTTPPort(portID int) (int32, error) {
	if s.minHTTPPort+int32(portID) >= s.maxHTTPPort {
		return 0, errors.New("unable to find an available HTTP port")
	}

	return s.minHTTPPort + int32(portID), nil
}

// getMappedPort returns the port associated with the given service information in the given port mapper.
func (s *ShadowServiceManager) getMappedPort(stateTable PortMapper, svcName, svcNamespace string, svcPort int32) (int32, error) {
	svc := k8s.ServiceWithPort{
		Namespace: svcNamespace,
		Name:      svcName,
		Port:      svcPort,
	}
	if port, ok := stateTable.Find(svc); ok {
		return port, nil
	}

	s.log.Debugf("No match found for %s/%s %d - Add a new port", svcName, svcNamespace, svcPort)

	port, err := stateTable.Add(&svc)
	if err != nil {
		return 0, fmt.Errorf("unable to add service to the TCP state table: %w", err)
	}

	s.log.Debugf("Service %s/%s %d as been assigned port %d", svcName, svcNamespace, svcPort, port)

	return port, nil
}

func isPortSuitable(trafficType string, sp corev1.ServicePort) bool {
	if trafficType == annotations.ServiceTypeUDP {
		return sp.Protocol == corev1.ProtocolUDP
	}

	if trafficType == annotations.ServiceTypeTCP || trafficType == annotations.ServiceTypeHTTP {
		return sp.Protocol == corev1.ProtocolTCP
	}

	return false
}

func parseKubernetesServerVersion(kubeClient kubernetes.Interface) (major, minor int) {
	kubeVersion, err := kubeClient.Discovery().ServerVersion()
	if err != nil {
		return 0, 0
	}

	major, err = strconv.Atoi(kubeVersion.Major)
	if err != nil {
		return 0, 0
	}

	minor, err = strconv.Atoi(kubeVersion.Minor)
	if err != nil {
		return 0, 0
	}

	return major, minor
}
