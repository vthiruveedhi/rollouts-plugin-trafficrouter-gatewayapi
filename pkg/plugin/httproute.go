package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/argoproj-labs/rollouts-plugin-trafficrouter-gatewayapi/internal/utils"
	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	pluginTypes "github.com/argoproj/argo-rollouts/utils/plugin/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	HTTPConfigMapKey = "httpManagedRoutes"
)

func (r *RpcPlugin) setHTTPRouteWeight(rollout *v1alpha1.Rollout, desiredWeight int32, gatewayAPIConfig *GatewayAPITrafficRouting) pluginTypes.RpcError {
	ctx := context.TODO()
	httpRouteClient := r.HTTPRouteClient
	if !r.IsTest {
		gatewayClientV1 := r.GatewayAPIClientset.GatewayV1()
		httpRouteClient = gatewayClientV1.HTTPRoutes(gatewayAPIConfig.Namespace)
	}

	// Get the current HTTPRoute
	httpRoute, err := httpRouteClient.Get(ctx, gatewayAPIConfig.HTTPRoute, metav1.GetOptions{})
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}

	canaryServiceName := rollout.Spec.Strategy.Canary.CanaryService
	stableServiceName := rollout.Spec.Strategy.Canary.StableService
	r.LogCtx.Logger.Info(fmt.Sprintf("Working with canary service %s and stable service %s", canaryServiceName, stableServiceName))

	// Determine if experiment step is active and extract experiment details
	experimentServices := make(map[string]int32)
	totalExperimentWeight := int32(0)
	isExperimentStep := false

	if rollout.Spec.Strategy.Canary.Steps != nil && rollout.Status.CurrentStepIndex != nil {
		currentStepIndex := *rollout.Status.CurrentStepIndex
		if currentStepIndex < int32(len(rollout.Spec.Strategy.Canary.Steps)) {
			currentStep := rollout.Spec.Strategy.Canary.Steps[currentStepIndex]
			if currentStep.Experiment != nil && currentStep.Experiment.Templates != nil {
				isExperimentStep = true
				r.LogCtx.Logger.Info("Found active experiment step")

				// Extract all template weights
				for _, template := range currentStep.Experiment.Templates {
					if template.Weight != nil {
						serviceName := fmt.Sprintf("%s-%s", rollout.Name, template.Name)
						weight := *template.Weight
						experimentServices[serviceName] = weight
						totalExperimentWeight += weight
						r.LogCtx.Logger.Info(fmt.Sprintf("Experiment service %s with weight %d", serviceName, weight))
					}
				}
			}
		}
	}

	// Calculate weight distribution
	stableWeight := int32(100)
	canaryWeight := desiredWeight

	if isExperimentStep && totalExperimentWeight > 0 {
		// Adjust weights to account for experiment services
		if desiredWeight > totalExperimentWeight {
			canaryWeight = desiredWeight - totalExperimentWeight
		} else {
			canaryWeight = 0
		}
		stableWeight = 100 - canaryWeight - totalExperimentWeight
	} else {
		// Standard canary deployment without experiments
		stableWeight = 100 - desiredWeight
	}

	r.LogCtx.Logger.Info(fmt.Sprintf("Weight distribution: stable=%d, canary=%d, experiment total=%d",
		stableWeight, canaryWeight, totalExperimentWeight))

	// Process each rule in the HTTPRoute
	for i := range httpRoute.Spec.Rules {
		rule := &httpRoute.Spec.Rules[i]

		// Track which services we've already processed in this rule
		processedServices := make(map[string]bool)

		// First pass: update weights for existing services
		updatedBackendRefs := make([]gatewayv1.HTTPBackendRef, 0)

		for _, backendRef := range rule.BackendRefs {
			serviceName := string(backendRef.Name)

			// Set weight based on service type
			switch serviceName {
			case canaryServiceName:
				weight := canaryWeight
				backendRef.Weight = &weight
				processedServices[serviceName] = true
				r.LogCtx.Logger.Info(fmt.Sprintf("Updating canary service %s weight to %d", serviceName, weight))

			case stableServiceName:
				weight := stableWeight
				backendRef.Weight = &weight
				processedServices[serviceName] = true
				r.LogCtx.Logger.Info(fmt.Sprintf("Updating stable service %s weight to %d", serviceName, weight))

			default:
				// Check if this is an experiment service
				if weight, isExperiment := experimentServices[serviceName]; isExperiment {
					backendRef.Weight = &weight
					processedServices[serviceName] = true
					r.LogCtx.Logger.Info(fmt.Sprintf("Updating experiment service %s weight to %d", serviceName, weight))
				}
			}

			updatedBackendRefs = append(updatedBackendRefs, backendRef)
		}

		// Second pass: check if we need to add any missing services to this rule
		// Only add services to rules that already have canary or stable service
		hasCanaryOrStable := processedServices[canaryServiceName] || processedServices[stableServiceName]

		if hasCanaryOrStable {
			// Add canary service if missing
			if !processedServices[canaryServiceName] && canaryWeight > 0 {
				r.LogCtx.Logger.Info(fmt.Sprintf("Adding missing canary service %s with weight %d", canaryServiceName, canaryWeight))
				updatedBackendRefs = append(updatedBackendRefs, createBackendRef(canaryServiceName, canaryWeight))
			}

			// Add stable service if missing
			if !processedServices[stableServiceName] && stableWeight > 0 {
				r.LogCtx.Logger.Info(fmt.Sprintf("Adding missing stable service %s with weight %d", stableServiceName, stableWeight))
				updatedBackendRefs = append(updatedBackendRefs, createBackendRef(stableServiceName, stableWeight))
			}

			// Add experiment services if missing
			for serviceName, weight := range experimentServices {
				if !processedServices[serviceName] && weight > 0 {
					r.LogCtx.Logger.Info(fmt.Sprintf("Adding experiment service %s with weight %d", serviceName, weight))
					updatedBackendRefs = append(updatedBackendRefs, createBackendRef(serviceName, weight))
				}
			}
		}

		// If we're not in an experiment step, remove any experiment services
		if !isExperimentStep {
			newBackendRefs := make([]gatewayv1.HTTPBackendRef, 0)
			for _, ref := range updatedBackendRefs {
				serviceName := string(ref.Name)
				// Keep only the canary and stable services
				if serviceName == canaryServiceName || serviceName == stableServiceName {
					newBackendRefs = append(newBackendRefs, ref)
				} else if strings.HasPrefix(serviceName, fmt.Sprintf("%s-experiment", rollout.Name)) {
					r.LogCtx.Logger.Info(fmt.Sprintf("Removing experiment service %s as experiment is no longer active", serviceName))
				} else {
					// Keep other unrelated services
					newBackendRefs = append(newBackendRefs, ref)
				}
			}
			updatedBackendRefs = newBackendRefs
		}

		// Update the rule with the modified backend references
		rule.BackendRefs = updatedBackendRefs
	}

	// Update the HTTPRoute
	r.LogCtx.Logger.Info(fmt.Sprintf("Updating HTTPRoute %s with modified rules", gatewayAPIConfig.HTTPRoute))
	updatedHTTPRoute, err := httpRouteClient.Update(ctx, httpRoute, metav1.UpdateOptions{})
	if r.IsTest {
		r.UpdatedHTTPRouteMock = updatedHTTPRoute
	}
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}

	r.LogCtx.Logger.Info("HTTPRoute updated successfully")
	return pluginTypes.RpcError{}
}

// Helper function to create a new backend reference
func createBackendRef(serviceName string, weight int32) gatewayv1.HTTPBackendRef {
	serviceKind := gatewayv1.Kind("Service")
	serviceGroup := gatewayv1.Group("")
	port := gatewayv1.PortNumber(80) // Consider extracting this from existing backends

	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Group: &serviceGroup,
				Kind:  &serviceKind,
				Name:  gatewayv1.ObjectName(serviceName),
				Port:  &port,
			},
			Weight: &weight,
		},
	}
}

func (r *RpcPlugin) setHTTPHeaderRoute(rollout *v1alpha1.Rollout, headerRouting *v1alpha1.SetHeaderRoute, gatewayAPIConfig *GatewayAPITrafficRouting) pluginTypes.RpcError {
	if headerRouting.Match == nil {
		managedRouteList := []v1alpha1.MangedRoutes{
			{
				Name: headerRouting.Name,
			},
		}
		return r.removeHTTPManagedRoutes(managedRouteList, gatewayAPIConfig)
	}
	ctx := context.TODO()
	httpRouteClient := r.HTTPRouteClient
	managedRouteMap := make(ManagedRouteMap)
	httpRouteName := gatewayAPIConfig.HTTPRoute
	clientset := r.TestClientset
	if !r.IsTest {
		gatewayClientv1 := r.GatewayAPIClientset.GatewayV1()
		httpRouteClient = gatewayClientv1.HTTPRoutes(gatewayAPIConfig.Namespace)
		clientset = r.Clientset.CoreV1().ConfigMaps(gatewayAPIConfig.Namespace)
	}
	configMap, err := utils.GetOrCreateConfigMap(gatewayAPIConfig.ConfigMap, utils.CreateConfigMapOptions{
		Clientset: clientset,
		Ctx:       ctx,
	})
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	err = utils.GetConfigMapData(configMap, HTTPConfigMapKey, &managedRouteMap)
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	httpRoute, err := httpRouteClient.Get(ctx, httpRouteName, metav1.GetOptions{})
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	canaryServiceName := gatewayv1.ObjectName(rollout.Spec.Strategy.Canary.CanaryService)
	stableServiceName := rollout.Spec.Strategy.Canary.StableService
	canaryServiceKind := gatewayv1.Kind("Service")
	canaryServiceGroup := gatewayv1.Group("")
	httpHeaderRouteRuleList, rpcError := getHTTPHeaderRouteRuleList(headerRouting)
	if rpcError.HasError() {
		return rpcError
	}
	httpRouteRuleList := HTTPRouteRuleList(httpRoute.Spec.Rules)
	backendRefNameList := []string{string(canaryServiceName), stableServiceName}
	httpRouteRule, err := getRouteRule(httpRouteRuleList, backendRefNameList...)
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	var canaryBackendRef *HTTPBackendRef
	for i := 0; i < len(httpRouteRule.BackendRefs); i++ {
		backendRef := httpRouteRule.BackendRefs[i]
		if canaryServiceName == backendRef.Name {
			canaryBackendRef = (*HTTPBackendRef)(&backendRef)
			break
		}
	}
	httpHeaderRouteRule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Group: &canaryServiceGroup,
						Kind:  &canaryServiceKind,
						Name:  canaryServiceName,
						Port:  canaryBackendRef.Port,
					},
				},
			},
		},
	}
	for i := 0; i < len(httpRouteRule.Matches); i++ {
		httpHeaderRouteRule.Matches = append(httpHeaderRouteRule.Matches, gatewayv1.HTTPRouteMatch{
			Path:        httpRouteRule.Matches[i].Path,
			Headers:     httpHeaderRouteRuleList,
			QueryParams: httpRouteRule.Matches[i].QueryParams,
		})
	}
	httpRouteRuleList = append(httpRouteRuleList, httpHeaderRouteRule)
	oldHTTPRuleList := httpRoute.Spec.Rules
	httpRoute.Spec.Rules = httpRouteRuleList
	oldConfigMapData := make(ManagedRouteMap)
	err = utils.GetConfigMapData(configMap, HTTPConfigMapKey, &oldConfigMapData)
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	taskList := []utils.Task{
		{
			Action: func() error {
				updatedHTTPRoute, err := httpRouteClient.Update(ctx, httpRoute, metav1.UpdateOptions{})
				if r.IsTest {
					r.UpdatedHTTPRouteMock = updatedHTTPRoute
				}
				if err != nil {
					return err
				}
				return nil
			},
			ReverseAction: func() error {
				httpRoute.Spec.Rules = oldHTTPRuleList
				updatedHTTPRoute, err := httpRouteClient.Update(ctx, httpRoute, metav1.UpdateOptions{})
				if r.IsTest {
					r.UpdatedHTTPRouteMock = updatedHTTPRoute
				}
				if err != nil {
					return err
				}
				return nil
			},
		},
		{
			Action: func() error {
				if managedRouteMap[headerRouting.Name] == nil {
					managedRouteMap[headerRouting.Name] = make(map[string]int)
				}
				managedRouteMap[headerRouting.Name][httpRouteName] = len(httpRouteRuleList) - 1
				err = utils.UpdateConfigMapData(configMap, managedRouteMap, utils.UpdateConfigMapOptions{
					Clientset:    clientset,
					ConfigMapKey: HTTPConfigMapKey,
					Ctx:          ctx,
				})
				if err != nil {
					return err
				}
				return nil
			},
			ReverseAction: func() error {
				err = utils.UpdateConfigMapData(configMap, oldConfigMapData, utils.UpdateConfigMapOptions{
					Clientset:    clientset,
					ConfigMapKey: HTTPConfigMapKey,
					Ctx:          ctx,
				})
				if err != nil {
					return err
				}
				return nil
			},
		},
	}
	err = utils.DoTransaction(r.LogCtx, taskList...)
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	return pluginTypes.RpcError{}
}

func getHTTPHeaderRouteRuleList(headerRouting *v1alpha1.SetHeaderRoute) ([]gatewayv1.HTTPHeaderMatch, pluginTypes.RpcError) {
	httpHeaderRouteRuleList := []gatewayv1.HTTPHeaderMatch{}
	for _, headerRule := range headerRouting.Match {
		httpHeaderRouteRule := gatewayv1.HTTPHeaderMatch{
			Name: gatewayv1.HTTPHeaderName(headerRule.HeaderName),
		}
		switch {
		case headerRule.HeaderValue.Exact != "":
			headerMatchType := gatewayv1.HeaderMatchExact
			httpHeaderRouteRule.Type = &headerMatchType
			httpHeaderRouteRule.Value = headerRule.HeaderValue.Exact
		case headerRule.HeaderValue.Prefix != "":
			headerMatchType := gatewayv1.HeaderMatchRegularExpression
			httpHeaderRouteRule.Type = &headerMatchType
			httpHeaderRouteRule.Value = headerRule.HeaderValue.Prefix + ".*"
		case headerRule.HeaderValue.Regex != "":
			headerMatchType := gatewayv1.HeaderMatchRegularExpression
			httpHeaderRouteRule.Type = &headerMatchType
			httpHeaderRouteRule.Value = headerRule.HeaderValue.Regex
		default:
			return nil, pluginTypes.RpcError{
				ErrorString: InvalidHeaderMatchTypeError,
			}
		}
		httpHeaderRouteRuleList = append(httpHeaderRouteRuleList, httpHeaderRouteRule)
	}
	return httpHeaderRouteRuleList, pluginTypes.RpcError{}
}

func (r *RpcPlugin) removeHTTPManagedRoutes(managedRouteNameList []v1alpha1.MangedRoutes, gatewayAPIConfig *GatewayAPITrafficRouting) pluginTypes.RpcError {
	ctx := context.TODO()
	httpRouteClient := r.HTTPRouteClient
	clientset := r.TestClientset
	httpRouteName := gatewayAPIConfig.HTTPRoute
	managedRouteMap := make(ManagedRouteMap)
	if !r.IsTest {
		gatewayClientv1 := r.GatewayAPIClientset.GatewayV1()
		httpRouteClient = gatewayClientv1.HTTPRoutes(gatewayAPIConfig.Namespace)
		clientset = r.Clientset.CoreV1().ConfigMaps(gatewayAPIConfig.Namespace)
	}
	configMap, err := utils.GetOrCreateConfigMap(gatewayAPIConfig.ConfigMap, utils.CreateConfigMapOptions{
		Clientset: clientset,
		Ctx:       ctx,
	})
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	err = utils.GetConfigMapData(configMap, HTTPConfigMapKey, &managedRouteMap)
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	httpRoute, err := httpRouteClient.Get(ctx, httpRouteName, metav1.GetOptions{})
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	httpRouteRuleList := HTTPRouteRuleList(httpRoute.Spec.Rules)
	isHTTPRouteRuleListChanged := false
	for _, managedRoute := range managedRouteNameList {
		managedRouteName := managedRoute.Name
		_, isOk := managedRouteMap[managedRouteName]
		if !isOk {
			r.LogCtx.Logger.Info(fmt.Sprintf("%s is not in httpHeaderManagedRouteMap", managedRouteName))
			continue
		}
		isHTTPRouteRuleListChanged = true
		httpRouteRuleList, err = removeManagedHTTPRouteEntry(managedRouteMap, httpRouteRuleList, managedRouteName, httpRouteName)
		if err != nil {
			return pluginTypes.RpcError{
				ErrorString: err.Error(),
			}
		}
	}
	if !isHTTPRouteRuleListChanged {
		return pluginTypes.RpcError{}
	}
	oldHTTPRuleList := httpRoute.Spec.Rules
	httpRoute.Spec.Rules = httpRouteRuleList
	oldConfigMapData := make(ManagedRouteMap)
	err = utils.GetConfigMapData(configMap, HTTPConfigMapKey, &oldConfigMapData)
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	taskList := []utils.Task{
		{
			Action: func() error {
				updatedHTTPRoute, err := httpRouteClient.Update(ctx, httpRoute, metav1.UpdateOptions{})
				if r.IsTest {
					r.UpdatedHTTPRouteMock = updatedHTTPRoute
				}
				if err != nil {
					return err
				}
				return nil
			},
			ReverseAction: func() error {
				httpRoute.Spec.Rules = oldHTTPRuleList
				updatedHTTPRoute, err := httpRouteClient.Update(ctx, httpRoute, metav1.UpdateOptions{})
				if r.IsTest {
					r.UpdatedHTTPRouteMock = updatedHTTPRoute
				}
				if err != nil {
					return err
				}
				return nil
			},
		},
		{
			Action: func() error {
				err = utils.UpdateConfigMapData(configMap, managedRouteMap, utils.UpdateConfigMapOptions{
					Clientset:    clientset,
					ConfigMapKey: HTTPConfigMapKey,
					Ctx:          ctx,
				})
				if err != nil {
					return err
				}
				return nil
			},
			ReverseAction: func() error {
				err = utils.UpdateConfigMapData(configMap, oldConfigMapData, utils.UpdateConfigMapOptions{
					Clientset:    clientset,
					ConfigMapKey: HTTPConfigMapKey,
					Ctx:          ctx,
				})
				if err != nil {
					return err
				}
				return nil
			},
		},
	}
	err = utils.DoTransaction(r.LogCtx, taskList...)
	if err != nil {
		return pluginTypes.RpcError{
			ErrorString: err.Error(),
		}
	}
	return pluginTypes.RpcError{}
}

func removeManagedHTTPRouteEntry(managedRouteMap ManagedRouteMap, routeRuleList HTTPRouteRuleList, managedRouteName string, httpRouteName string) (HTTPRouteRuleList, error) {
	routeManagedRouteMap, isOk := managedRouteMap[managedRouteName]
	if !isOk {
		return nil, fmt.Errorf(ManagedRouteMapEntryDeleteError, managedRouteName, managedRouteName)
	}
	managedRouteIndex, isOk := routeManagedRouteMap[httpRouteName]
	if !isOk {
		managedRouteMapKey := managedRouteName + "." + httpRouteName
		return nil, fmt.Errorf(ManagedRouteMapEntryDeleteError, managedRouteMapKey, managedRouteMapKey)
	}
	delete(routeManagedRouteMap, httpRouteName)
	if len(managedRouteMap[managedRouteName]) == 0 {
		delete(managedRouteMap, managedRouteName)
	}
	for _, currentRouteManagedRouteMap := range managedRouteMap {
		value := currentRouteManagedRouteMap[httpRouteName]
		if value > managedRouteIndex {
			currentRouteManagedRouteMap[httpRouteName]--
		}
	}
	routeRuleList = slices.Delete(routeRuleList, managedRouteIndex, managedRouteIndex+1)
	return routeRuleList, nil
}

func (r *HTTPRouteRule) Iterator() (GatewayAPIRouteRuleIterator[*HTTPBackendRef], bool) {
	backendRefList := r.BackendRefs
	index := 0
	next := func() (*HTTPBackendRef, bool) {
		if len(backendRefList) == index {
			return nil, false
		}
		backendRef := (*HTTPBackendRef)(&backendRefList[index])
		index = index + 1
		return backendRef, len(backendRefList) > index
	}
	return next, len(backendRefList) > index
}

func (r HTTPRouteRuleList) Iterator() (GatewayAPIRouteRuleListIterator[*HTTPBackendRef, *HTTPRouteRule], bool) {
	routeRuleList := r
	index := 0
	next := func() (*HTTPRouteRule, bool) {
		if len(routeRuleList) == index {
			return nil, false
		}
		routeRule := (*HTTPRouteRule)(&routeRuleList[index])
		index++
		return routeRule, len(routeRuleList) > index
	}
	return next, len(routeRuleList) != index
}

func (r HTTPRouteRuleList) Error() error {
	return errors.New(BackendRefWasNotFoundInHTTPRouteError)
}

func (r *HTTPBackendRef) GetName() string {
	return string(r.Name)
}

func (r HTTPRoute) GetName() string {
	return r.Name
}
