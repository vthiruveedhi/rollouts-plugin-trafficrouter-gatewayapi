package plugin

import (
	"context"
	"fmt"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayApiClientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// HandleExperiment adds experiment services to the HTTPRoute if an experiment is active
// or removes them if the experiment has completed
func HandleExperiment(ctx context.Context, clientset *kubernetes.Clientset, gatewayClient *gatewayApiClientset.Clientset, logger *logrus.Entry, rollout *v1alpha1.Rollout, httpRoute *gatewayv1.HTTPRoute) error {
	// Find the matching rule for our rollout first (needed for both adding and removing)
	ruleIdx := -1
	stableService := rollout.Spec.Strategy.Canary.StableService
	canaryService := rollout.Spec.Strategy.Canary.CanaryService

	for i, rule := range httpRoute.Spec.Rules {
		if ruleIdx != -1 {
			break
		}
		for _, backendRef := range rule.BackendRefs {
			if string(backendRef.Name) == stableService || string(backendRef.Name) == canaryService {
				ruleIdx = i
				break
			}
		}
	}

	if ruleIdx == -1 {
		return fmt.Errorf("no matching rule found for rollout %s", rollout.Name)
	}

	// Check if experiment is active in the rollout
	isExperimentActive := rollout.Spec.Strategy.Canary != nil && rollout.Status.Canary.CurrentExperiment != ""

	// Check if we have experiment services in the HTTPRoute
	hasExperimentServices := false
	for _, backendRef := range httpRoute.Spec.Rules[ruleIdx].BackendRefs {
		// Identify experiment services (they'll be different from stable and canary)
		serviceName := string(backendRef.Name)
		if serviceName != stableService && serviceName != canaryService {
			hasExperimentServices = true
			break
		}
	}

	// CASE 1: Experiment is active - add or update experiment services
	if isExperimentActive {
		logger.Info(fmt.Sprintf("Found active experiment %s", rollout.Status.Canary.CurrentExperiment))

		// Get the experiment services from the rollout status
		if len(rollout.Status.Canary.Weights.Additional) == 0 {
			logger.Info("No experiment services found in rollout status, skipping experiment service addition")
			return nil
		}

		// First, update the stable service weight to ensure proper traffic distribution
		stableWeight := int32(45) // Default to 45% for the stable service when experiments are active
		for i, backendRef := range httpRoute.Spec.Rules[ruleIdx].BackendRefs {
			if string(backendRef.Name) == stableService {
				httpRoute.Spec.Rules[ruleIdx].BackendRefs[i].Weight = &stableWeight
				break
			}
		}

		// Process each additional service (these are the experiment services)
		for _, additionalDestination := range rollout.Status.Canary.Weights.Additional {
			serviceName := additionalDestination.ServiceName
			weight := additionalDestination.Weight

			// Check if this service is already in the backend refs
			exists := false
			for _, backendRef := range httpRoute.Spec.Rules[ruleIdx].BackendRefs {
				if string(backendRef.Name) == serviceName {
					exists = true
					break
				}
			}

			if !exists {
				logger.Info(fmt.Sprintf("Adding experiment service to HTTPRoute: %s with weight %d", serviceName, weight))

				// Get the actual service port by querying the Kubernetes API
				service, err := clientset.CoreV1().Services(rollout.Namespace).Get(ctx, serviceName, metav1.GetOptions{})
				if err != nil {
					logger.Warn(fmt.Sprintf("Failed to get service %s: %v", serviceName, err))
					continue
				}

				// Default to 8080 if we can't determine the port
				port := gatewayv1.PortNumber(8080)

				// Find port by service port name
				portName := "http" // Common name for HTTP ports
				for _, servicePort := range service.Spec.Ports {
					if servicePort.Name == portName {
						port = gatewayv1.PortNumber(servicePort.Port)
						break
					}
				}

				// If no named port found, use the first port
				if len(service.Spec.Ports) > 0 && port == 8080 {
					port = gatewayv1.PortNumber(service.Spec.Ports[0].Port)
				}

				// Add the experiment service to the backend refs
				namespace := gatewayv1.Namespace(rollout.Namespace)
				httpRoute.Spec.Rules[ruleIdx].BackendRefs = append(httpRoute.Spec.Rules[ruleIdx].BackendRefs, gatewayv1.HTTPBackendRef{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name:      gatewayv1.ObjectName(serviceName),
							Namespace: &namespace,
							Port:      &port,
						},
						Weight: &weight,
					},
				})
			}
		}
		return nil
	}

	// CASE 2: Experiment is not active but we have experiment services - clean them up
	if !isExperimentActive && hasExperimentServices {
		logger.Info("Experiment is no longer active, removing experiment services from HTTPRoute")

		// Reset the stable service weight back to 100
		stableWeight := int32(100)

		// Create a new backend refs slice with only stable and canary services
		filteredBackendRefs := []gatewayv1.HTTPBackendRef{}

		for _, backendRef := range httpRoute.Spec.Rules[ruleIdx].BackendRefs {
			serviceName := string(backendRef.Name)

			if serviceName == stableService {
				// Keep stable service but update its weight
				backendRef.Weight = &stableWeight
				filteredBackendRefs = append(filteredBackendRefs, backendRef)
			} else if serviceName == canaryService {
				// Keep canary service with weight 0
				zeroWeight := int32(0)
				backendRef.Weight = &zeroWeight
				filteredBackendRefs = append(filteredBackendRefs, backendRef)
			} else {
				// Skip other services (experiment services)
				logger.Info(fmt.Sprintf("Removing experiment service from HTTPRoute: %s", serviceName))
			}
		}

		// Replace the backend refs with our filtered list
		httpRoute.Spec.Rules[ruleIdx].BackendRefs = filteredBackendRefs
		logger.Info("Experiment services removed from HTTPRoute")
	}

	return nil
}
