// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
)

const (
	fixtureConfigMapName = "oidc-probe"
	fixtureRoleName      = "oidc-probe-reader"
	fixtureBindingName   = "oidc-probe-reader"
	cleanupTimeout       = 10 * time.Second
)

// AdminFixture is the isolated namespace and narrow RBAC grant used by a token
// probe. The caller must call Cleanup after the probe completes.
type AdminFixture struct {
	Namespace      string
	ConfigMapName  string
	AllowedRequest contracts.ResourceRequest
	DeniedRequest  contracts.ResourceRequest
}

// PrepareAdminFixture creates an isolated namespace, a ConfigMap, and a Role
// that lets username list ConfigMaps but not Secrets. This privileged setup is
// deliberately separate from Probe.
func PrepareAdminFixture(ctx context.Context, admin kubernetes.Interface, namespace, username string) (AdminFixture, error) {
	if admin == nil {
		return AdminFixture{}, fmt.Errorf("admin client is required")
	}
	if namespace == "" {
		return AdminFixture{}, fmt.Errorf("namespace is required")
	}
	if username == "" {
		return AdminFixture{}, fmt.Errorf("username is required")
	}

	if _, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		return AdminFixture{}, fmt.Errorf("create probe namespace %q: %w", namespace, err)
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = admin.CoreV1().Namespaces().Delete(cleanupCtx, namespace, metav1.DeleteOptions{})
	}
	if _, err := admin.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: fixtureConfigMapName},
		Data:       map[string]string{"fixture": "oidc probe"},
	}, metav1.CreateOptions{}); err != nil {
		cleanup()
		return AdminFixture{}, fmt.Errorf("create probe ConfigMap: %w", err)
	}
	if _, err := admin.RbacV1().Roles(namespace).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: fixtureRoleName},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{resourceConfigMaps},
			Verbs:     []string{verbList},
		}},
	}, metav1.CreateOptions{}); err != nil {
		cleanup()
		return AdminFixture{}, fmt.Errorf("create probe Role: %w", err)
	}
	if _, err := admin.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: fixtureBindingName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: username}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: fixtureRoleName},
	}, metav1.CreateOptions{}); err != nil {
		cleanup()
		return AdminFixture{}, fmt.Errorf("create probe RoleBinding: %w", err)
	}

	return AdminFixture{
		Namespace:      namespace,
		ConfigMapName:  fixtureConfigMapName,
		AllowedRequest: contracts.ResourceRequest{Verb: verbList, Resource: resourceConfigMaps, Namespace: namespace},
		DeniedRequest:  contracts.ResourceRequest{Verb: verbList, Resource: resourceSecrets, Namespace: namespace},
	}, nil
}

// Cleanup removes the fixture namespace and its resources.
func (f AdminFixture) Cleanup(ctx context.Context, admin kubernetes.Interface) error {
	if admin == nil {
		return fmt.Errorf("admin client is required")
	}
	if f.Namespace == "" {
		return fmt.Errorf("fixture namespace is required")
	}
	if err := admin.CoreV1().Namespaces().Delete(ctx, f.Namespace, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete probe namespace %q: %w", f.Namespace, err)
	}
	return nil
}
