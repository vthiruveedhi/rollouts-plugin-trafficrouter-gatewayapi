package plugin

import (
	"context"
	"fmt"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayApiClientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// HandleExperiment adds experiment services to the HTTPRoute if an experiment is active
func HandleExperiment(ctx context.Context, clientset *kubernetes.Clientset, gatewayClient *gatewayApiClientset.Clientset, logger *logrus.Entry, rollout *v1alpha1.Rollout, httpRoute *gatewayv1.HTTPRoute) error {
	// Check if experiment is enabled in the rollout
	if rollout.Spec.Strategy.Canary == nil || rollout.Status.Canary.CurrentExperiment == "" {
		return nil // No active experiment
	}

	logger.Info(fmt.Sprintf("Found active experiment %s", rollout.Status.Canary.CurrentExperiment))

	// Find the matching rule for our rollout
	ruleIdx := -1
	stableService := rollout.Spec.Strategy.Canary.StableService
	canaryService := rollout.Spec.Strategy.Canary.CanaryService

	for i, rule := range httpRoute.Spec.Rules {
		if ruleIdx != -1 {
			break
		}
		for _, backendRef := range rule.BackendRefs {
			if backendRef.Name == gatewayv1.ObjectName(stableService) || backendRef.Name == gatewayv1.ObjectName(canaryService) {
				ruleIdx = i
				break
			}
		}
	}

	if ruleIdx == -1 {
		return fmt.Errorf("no matching rule found for rollout %s", rollout.Name)
	}

	// Get the experiment services from the rollout status
	if len(rollout.Status.Canary.Weights.Additional) == 0 {
		logger.Info("No experiment services found in rollout status, skipping experiment service addition")
		return nil
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

			// Add the experiment service to the backend refs
			port := gatewayv1.PortNumber(80) // Default port - adjust as needed
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
