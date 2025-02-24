/*
Copyright 2020 The cert-manager Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vault

import (
	"context"
	"fmt"
	"path"

	vault "github.com/hashicorp/vault/api"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const vaultToken = "vault-root-token"

// VaultInitializer holds the state of a configured Vault PKI. We use the same
// Vault server for all tests. PKIs are mounted and unmounted for each test
// scenario that uses them.
type VaultInitializer struct {
	client *vault.Client
	proxy  *proxy

	Details

	RootMount         string
	IntermediateMount string
	// Whether the intermediate CA should be configured with root CA
	ConfigureWithRoot  bool
	Role               string // AppRole auth Role
	AppRoleAuthPath    string // AppRole auth mount point in Vault
	KubernetesAuthPath string // Kubernetes auth mount point in Vault
	APIServerURL       string // Kubernetes API Server URL
	APIServerCA        string // Kubernetes API Server CA certificate
}

func NewVaultAppRoleSecret(secretName, secretId string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName,
		},
		StringData: map[string]string{
			"secretkey": secretId,
		},
	}
}

func NewVaultKubernetesSecret(secretName, serviceAccountName string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName,
			Annotations: map[string]string{
				"kubernetes.io/service-account.name": serviceAccountName,
			},
		},
		Type: "kubernetes.io/service-account-token",
	}
}

// Set up a new Vault client, port-forward to the Vault instance.
func (v *VaultInitializer) Init() error {
	if v.AppRoleAuthPath == "" {
		v.AppRoleAuthPath = "approle"
	}

	if v.KubernetesAuthPath == "" {
		v.KubernetesAuthPath = "kubernetes"
	}

	v.proxy = newProxy(v.PodNS, v.PodName, v.Kubectl, v.VaultCA)
	client, err := v.proxy.init()
	if err != nil {
		return err
	}
	v.client = client

	return nil
}

// Set up a Vault PKI.
func (v *VaultInitializer) Setup() error {
	// Enable a new Vault secrets engine at v.RootMount
	if err := v.mountPKI(v.RootMount, "87600h"); err != nil {
		return err
	}

	// Generate a self-signed CA cert using the engine at v.RootMount
	rootCa, err := v.generateRootCert()
	if err != nil {
		return err
	}

	// Configure issuing certificate endpoints and CRL distribution points to be
	// set on certs issued by v.RootMount.
	if err := v.configureCert(v.RootMount); err != nil {
		return err

	}

	// Enable a new Vault secrets engine at v.IntermediateMount
	if err := v.mountPKI(v.IntermediateMount, "43800h"); err != nil {
		return err
	}

	// Generate a CSR for secrets engine at v.IntermediateMount
	csr, err := v.generateIntermediateSigningReq()
	if err != nil {
		return err
	}

	// Issue a new intermediate CA from v.RootMount for the CSR created above.
	intermediateCa, err := v.signCertificate(csr)
	if err != nil {
		return err
	}

	// Set the engine at v.IntermediateMount as an intermediateCA using the cert
	// issued by v.RootMount, above and optionally the root CA cert.
	caChain := intermediateCa
	if v.ConfigureWithRoot {
		caChain = fmt.Sprintf("%s\n%s", intermediateCa, rootCa)
	}
	if err := v.importSignIntermediate(caChain, v.IntermediateMount); err != nil {
		return err
	}

	// Configure issuing certificate endpoints and CRL distribution points to be
	// set on certs issued by v.IntermediateMount.
	if err := v.configureCert(v.IntermediateMount); err != nil {
		return err
	}

	if err := v.setupRole(); err != nil {
		return err
	}

	if err := v.setupKubernetesBasedAuth(); err != nil {
		return err
	}

	return nil
}

func (v *VaultInitializer) Clean() error {
	if err := v.client.Sys().Unmount("/" + v.IntermediateMount); err != nil {
		return fmt.Errorf("unable to unmount %v: %v", v.IntermediateMount, err)
	}
	if err := v.client.Sys().Unmount("/" + v.RootMount); err != nil {
		return fmt.Errorf("unable to unmount %v: %v", v.RootMount, err)
	}

	v.proxy.clean()

	return nil
}

func (v *VaultInitializer) CreateAppRole() (string, string, error) {
	// create policy
	role_path := path.Join(v.IntermediateMount, "sign", v.Role)
	policy := fmt.Sprintf("path \"%s\" { capabilities = [ \"create\", \"update\" ] }", role_path)
	err := v.client.Sys().PutPolicy(v.Role, policy)
	if err != nil {
		return "", "", fmt.Errorf("error creating policy: %s", err.Error())
	}

	// # create approle
	params := map[string]string{
		"period":   "24h",
		"policies": v.Role,
	}

	baseUrl := path.Join("/v1", "auth", v.AppRoleAuthPath, "role", v.Role)
	_, err = v.proxy.callVault("POST", baseUrl, "", params)
	if err != nil {
		return "", "", fmt.Errorf("error creating approle: %s", err.Error())
	}

	// # read the role-id
	url := path.Join(baseUrl, "role-id")
	roleId, err := v.proxy.callVault("GET", url, "role_id", map[string]string{})
	if err != nil {
		return "", "", fmt.Errorf("error reading role_id: %s", err.Error())
	}

	// # read the secret-id
	url = path.Join(baseUrl, "secret-id")
	secretId, err := v.proxy.callVault("POST", url, "secret_id", map[string]string{})
	if err != nil {
		return "", "", fmt.Errorf("error reading secret_id: %s", err.Error())
	}

	return roleId, secretId, nil
}

func (v *VaultInitializer) CleanAppRole() error {
	url := path.Join("/v1", "auth", v.AppRoleAuthPath, "role", v.Role)
	_, err := v.proxy.callVault("DELETE", url, "", map[string]string{})
	if err != nil {
		return fmt.Errorf("error deleting AppRole: %s", err.Error())
	}

	err = v.client.Sys().DeletePolicy(v.Role)
	if err != nil {
		return fmt.Errorf("error deleting policy: %s", err.Error())
	}

	return nil
}

func (v *VaultInitializer) mountPKI(mount, ttl string) error {
	opts := &vault.MountInput{
		Type: "pki",
		Config: vault.MountConfigInput{
			MaxLeaseTTL: "87600h",
		},
	}
	if err := v.client.Sys().Mount("/"+mount, opts); err != nil {
		return fmt.Errorf("error mounting %s: %s", mount, err.Error())
	}

	return nil
}

func (v *VaultInitializer) generateRootCert() (string, error) {
	params := map[string]string{
		"common_name":          "Root CA",
		"ttl":                  "87600h",
		"exclude_cn_from_sans": "true",
		"key_type":             "ec",
		"key_bits":             "256",
	}
	url := path.Join("/v1", v.RootMount, "root", "generate", "internal")

	cert, err := v.proxy.callVault("POST", url, "certificate", params)
	if err != nil {
		return "", fmt.Errorf("error generating CA root certificate: %s", err.Error())
	}

	return cert, nil
}

func (v *VaultInitializer) generateIntermediateSigningReq() (string, error) {
	params := map[string]string{
		"common_name":          "Intermediate CA",
		"ttl":                  "43800h",
		"exclude_cn_from_sans": "true",
		"key_type":             "ec",
		"key_bits":             "256",
	}
	url := path.Join("/v1", v.IntermediateMount, "intermediate", "generate", "internal")

	csr, err := v.proxy.callVault("POST", url, "csr", params)
	if err != nil {
		return "", fmt.Errorf("error generating CA intermediate certificate: %s", err.Error())
	}

	return csr, nil
}

func (v *VaultInitializer) signCertificate(csr string) (string, error) {
	params := map[string]string{
		"use_csr_values":       "true",
		"ttl":                  "43800h",
		"exclude_cn_from_sans": "true",
		"csr":                  csr,
	}
	url := path.Join("/v1", v.RootMount, "root", "sign-intermediate")

	cert, err := v.proxy.callVault("POST", url, "certificate", params)
	if err != nil {
		return "", fmt.Errorf("error signing intermediate Vault certificate: %s", err.Error())
	}

	return cert, nil
}

func (v *VaultInitializer) importSignIntermediate(caChain, intermediateMount string) error {
	params := map[string]string{
		"certificate": caChain,
	}
	url := path.Join("/v1", intermediateMount, "intermediate", "set-signed")

	_, err := v.proxy.callVault("POST", url, "", params)
	if err != nil {
		return fmt.Errorf("error importing intermediate Vault certificate: %s", err.Error())
	}

	return nil
}

func (v *VaultInitializer) configureCert(mount string) error {
	params := map[string]string{
		"issuing_certificates":    fmt.Sprintf("https://vault.vault:8200/v1/%s/ca", mount),
		"crl_distribution_points": fmt.Sprintf("https://vault.vault:8200/v1/%s/crl", mount),
	}
	url := path.Join("/v1", mount, "config", "urls")

	_, err := v.proxy.callVault("POST", url, "", params)
	if err != nil {
		return fmt.Errorf("error configuring Vault certificate: %s", err.Error())
	}

	return nil
}

func (v *VaultInitializer) setupRole() error {
	// vault auth-enable approle
	auths, err := v.client.Sys().ListAuth()
	if err != nil {
		return fmt.Errorf("error fetching auth mounts: %s", err.Error())
	}

	if _, ok := auths[v.AppRoleAuthPath]; !ok {
		options := &vault.EnableAuthOptions{Type: "approle"}
		if err := v.client.Sys().EnableAuthWithOptions(v.AppRoleAuthPath, options); err != nil {
			return fmt.Errorf("error enabling approle: %s", err.Error())
		}
	}

	params := map[string]string{
		"allow_any_name":     "true",
		"max_ttl":            "2160h",
		"key_type":           "any",
		"require_cn":         "false",
		"allowed_uri_sans":   "spiffe://cluster.local/*",
		"enforce_hostnames":  "false",
		"allow_bare_domains": "true",
	}
	url := path.Join("/v1", v.IntermediateMount, "roles", v.Role)

	_, err = v.proxy.callVault("POST", url, "", params)
	if err != nil {
		return fmt.Errorf("error creating role %s: %s", v.Role, err.Error())
	}

	return nil
}

func (v *VaultInitializer) setupKubernetesBasedAuth() error {
	if len(v.APIServerURL) == 0 {
		// skip initialization if not provided
		return nil
	}

	// vault auth-enable kubernetes
	auths, err := v.client.Sys().ListAuth()
	if err != nil {
		return fmt.Errorf("error fetching auth mounts: %s", err.Error())
	}

	if _, ok := auths[v.KubernetesAuthPath]; !ok {
		options := &vault.EnableAuthOptions{Type: "kubernetes"}
		if err := v.client.Sys().EnableAuthWithOptions(v.KubernetesAuthPath, options); err != nil {
			return fmt.Errorf("error enabling kubernetes auth: %s", err.Error())
		}
	}

	// vault write auth/kubernetes/config
	params := map[string]string{
		"kubernetes_host":    v.APIServerURL,
		"kubernetes_ca_cert": v.APIServerCA,
		// Since Vault 1.9, HashiCorp recommends disabling the iss validation.
		// If we don't disable the iss validation, we can't use the same
		// Kubernetes auth config for both testing the "secretRef" Kubernetes
		// auth and the "serviceAccountRef" Kubernetes auth because the former
		// relies on static tokens for which "iss" is
		// "kubernetes/serviceaccount", and the later relies on bound tokens for
		// which "iss" is "https://kubernetes.default.svc.cluster.local".
		// https://www.vaultproject.io/docs/auth/kubernetes#kubernetes-1-21
		"disable_iss_validation": "true",
	}

	url := fmt.Sprintf("/v1/auth/%s/config", v.KubernetesAuthPath)
	_, err = v.proxy.callVault("POST", url, "", params)

	if err != nil {
		return fmt.Errorf("error configuring kubernetes auth backend: %s", err.Error())
	}

	return nil
}

func roleName(podNS, podSA string) string {
	return fmt.Sprintf("auth-delegator:%s:%s", podNS, podSA)
}

// CreateKubernetesRole creates a service account and ClusterRoleBinding for
// Kubernetes auth delegation. The name "boundSA" refers to the Vault param
// "bound_service_account_names".
func (v *VaultInitializer) CreateKubernetesRole(client kubernetes.Interface, vaultRole, boundNS, boundSA string) error {
	// Watch out, we refer to two different namespaces here:
	//  - v.PodNS = the pod's service account used by Vault's pod to
	//    authenticate with Kubernetes for the token review.
	//  - boundSA = the service account used to login using the Vault Kubernetes
	//    auth.
	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleName(v.PodNS, v.PodSA),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"authentication.k8s.io"},
				Resources: []string{"tokenreviews"},
				Verbs:     []string{"create"},
			},
			{
				APIGroups: []string{"authorization.k8s.io"},
				Resources: []string{"subjectaccessreviews"},
				Verbs:     []string{"create"},
			},
		},
	}
	_, err := client.RbacV1().ClusterRoles().Create(context.TODO(), clusterRole, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating Role for Kubernetes auth ServiceAccount: %s", err.Error())
	}

	roleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleName(v.PodNS, v.PodSA),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRole.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				Name:      v.PodSA,
				Kind:      "ServiceAccount",
				Namespace: v.PodNS,
			},
		},
	}
	_, err = client.RbacV1().ClusterRoleBindings().Create(context.TODO(), roleBinding, metav1.CreateOptions{})

	if err != nil {
		return fmt.Errorf("error creating RoleBinding for Kubernetes auth ServiceAccount: %s", err.Error())
	}

	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: boundSA,
		},
	}
	_, err = client.CoreV1().ServiceAccounts(boundNS).Create(context.TODO(), serviceAccount, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating ServiceAccount for Kubernetes auth: %s", err.Error())
	}

	// vault write auth/kubernetes/role/<roleName>
	roleParams := map[string]string{
		"bound_service_account_names":      boundSA,
		"bound_service_account_namespaces": boundNS,
		"policies":                         "[" + v.Role + "]",
	}

	url := path.Join(fmt.Sprintf("/v1/auth/%s/role", v.KubernetesAuthPath), vaultRole)
	_, err = v.proxy.callVault("POST", url, "", roleParams)
	if err != nil {
		return fmt.Errorf("error configuring kubernetes auth role: %s", err.Error())
	}

	params := map[string]string{
		"allow_any_name":                   "true",
		"max_ttl":                          "2160h",
		"key_type":                         "any",
		"require_cn":                       "false",
		"allowed_uri_sans":                 "spiffe://cluster.local/*",
		"enforce_hostnames":                "false",
		"allow_bare_domains":               "true",
		"bound_service_account_names":      boundSA,
		"bound_service_account_namespaces": boundNS,
	}
	url = path.Join("/v1", v.IntermediateMount, "roles", v.Role)

	_, err = v.proxy.callVault("POST", url, "", params)
	if err != nil {
		return fmt.Errorf("error creating role %s: %s", v.Role, err.Error())
	}

	// create policy
	role_path := path.Join(v.IntermediateMount, "sign", v.Role)
	policy := fmt.Sprintf(`path "%s" { capabilities = [ "create", "update" ] }`, role_path)
	err = v.client.Sys().PutPolicy(v.Role, policy)
	if err != nil {
		return fmt.Errorf("error creating policy: %s", err.Error())
	}

	// # create approle
	params = map[string]string{
		"period":                           "24h",
		"policies":                         v.Role,
		"bound_service_account_names":      boundSA,
		"bound_service_account_namespaces": boundNS,
	}

	baseUrl := path.Join("/v1", "auth", v.KubernetesAuthPath, "role", v.Role)
	_, err = v.proxy.callVault("POST", baseUrl, "", params)
	if err != nil {
		return fmt.Errorf("error creating kubernetes role: %s", err.Error())
	}

	return nil
}

// CleanKubernetesRole cleans up the ClusterRoleBinding and ServiceAccount for Kubernetes auth delegation
func (v *VaultInitializer) CleanKubernetesRole(client kubernetes.Interface, vaultRole, boundNS, boundSA string) error {
	if err := client.RbacV1().ClusterRoleBindings().Delete(context.TODO(), roleName(v.PodNS, v.PodSA), metav1.DeleteOptions{}); err != nil {
		return err
	}

	if err := client.RbacV1().ClusterRoles().Delete(context.TODO(), roleName(v.PodNS, v.PodSA), metav1.DeleteOptions{}); err != nil {
		return err
	}

	if err := client.CoreV1().ServiceAccounts(boundNS).Delete(context.TODO(), boundSA, metav1.DeleteOptions{}); err != nil {
		return err
	}

	// vault delete auth/kubernetes/role/<roleName>
	url := path.Join(fmt.Sprintf("/v1/auth/%s/role", v.KubernetesAuthPath), vaultRole)
	_, err := v.proxy.callVault("DELETE", url, "", nil)
	if err != nil {
		return fmt.Errorf("error cleaning up kubernetes auth role: %s", err.Error())
	}

	return nil
}

func RoleAndBindingForServiceAccountRefAuth(roleName, namespace, serviceAccount string) (*rbacv1.Role, *rbacv1.RoleBinding) {
	return &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      roleName,
				Namespace: namespace,
			},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups:     []string{""},
					Resources:     []string{"serviceaccounts/token"},
					ResourceNames: []string{serviceAccount},
					Verbs:         []string{"create"},
				},
			},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: roleName,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     roleName,
			},
			Subjects: []rbacv1.Subject{
				{
					Name:      "cert-manager",
					Namespace: "cert-manager",
					Kind:      "ServiceAccount",
				},
			},
		}
}

// CreateKubernetesRoleForServiceAccountRefAuth creates a service account and a
// role for using the "serviceAccountRef" field.
func CreateKubernetesRoleForServiceAccountRefAuth(client kubernetes.Interface, roleName, saNS, saName string) error {
	role, binding := RoleAndBindingForServiceAccountRefAuth(roleName, saNS, saName)
	_, err := client.RbacV1().Roles(saNS).Create(context.TODO(), role, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating Role for Kubernetes auth ServiceAccount with serviceAccountRef: %s", err.Error())
	}
	_, err = client.RbacV1().RoleBindings(saNS).Create(context.TODO(), binding, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating RoleBinding for Kubernetes auth ServiceAccount with serviceAccountRef: %s", err.Error())
	}

	return nil
}

func CleanKubernetesRoleForServiceAccountRefAuth(client kubernetes.Interface, roleName, saNS, saName string) error {
	if err := client.RbacV1().RoleBindings(saNS).Delete(context.TODO(), roleName, metav1.DeleteOptions{}); err != nil {
		return err
	}

	if err := client.RbacV1().Roles(saNS).Delete(context.TODO(), roleName, metav1.DeleteOptions{}); err != nil {
		return err
	}

	if err := client.CoreV1().ServiceAccounts(saNS).Delete(context.TODO(), saName, metav1.DeleteOptions{}); err != nil {
		return err
	}

	return nil
}
