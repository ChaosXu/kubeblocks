/*
Copyright (C) 2022-2025 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package constant

const (
	APIGroup = "kubeblocks.io"
	AppName  = "kubeblocks"

	RBACRoleName = "kubeblocks-cluster-pod-role"
)

const (
	KBServiceAccountName = "KUBEBLOCKS_SERVICEACCOUNT_NAME"
	KBToolsImage         = "KUBEBLOCKS_TOOLS_IMAGE"
	KBImagePullPolicy    = "KUBEBLOCKS_IMAGE_PULL_POLICY"
	KBImagePullSecrets   = "KUBEBLOCKS_IMAGE_PULL_SECRETS"
)

const (
	StatefulSetKind           = "StatefulSet"
	PodKind                   = "Pod"
	JobKind                   = "Job"
	VolumeSnapshotKind        = "VolumeSnapshot"
	ServiceKind               = "Service"
	SecretKind                = "Secret"
	ConfigMapKind             = "ConfigMap"
	PersistentVolumeClaimKind = "PersistentVolumeClaim"
	PersistentVolumeKind      = "PersistentVolume"
	ComponentParameterKind    = "ComponentParameter"
	RoleBindingKind           = "RoleBinding"
	ServiceAccountKind        = "ServiceAccount"
	EventKind                 = "Event"
)

// username and password are keys in created secrets for others to refer to.
const (
	AccountNameForSecret   = "username"
	AccountPasswdForSecret = "password"
)

const (
	KubernetesClusterDomainEnv = "KUBERNETES_CLUSTER_DOMAIN"
	DefaultDNSDomain           = "cluster.local"
)

const (
	KBPrefix      = "KB"
	KBLowerPrefix = "kb"
)

const InvalidContainerPort int32 = 0

const EmptyInsTemplateName = ""

type Key string

// DryRunContextKey tells the KB Controllers to do dry-run reconciliations
const DryRunContextKey Key = "dry-run"

// APIVersionSupported specifies the API version supported by the operator.
const (
	APIVersionSupported = "api-version-supported"
)

const (
	ReleasePhaseStable string = "stable"
	ReleasePhaseCanary string = "canary"
)
