/*
Copyright The Treebind Authors.

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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

const (
	// discoveryPath is where the API server publishes the issuer of the tokens it
	// signs. Every service account may read it by default, so the operator needs
	// no permission of its own for it.
	discoveryPath = "/.well-known/openid-configuration"
	// discoveryTimeout bounds the one request made at start.
	discoveryTimeout = 15 * time.Second

	// serviceAccountPrefix starts the subject of a service account's token.
	serviceAccountPrefix = "system:serviceaccount:"
)

// rawGetter reads a path of the API server.
type rawGetter func(ctx context.Context, path string) ([]byte, error)

// statusSettings are the two facts about the operator's token that the sync pods
// need to check it (docs/adr/0011): who issued it and whom it names.
type statusSettings struct {
	Issuer, Subject string
}

// resolveStatusSettings returns the issuer and the subject for the sync pods. A
// value that is given is used as it is, which is the way out on a cluster whose
// discovery document is not readable. Otherwise the issuer is the one the API
// server publishes, and the subject is the service account that the operator pod
// runs as, which the Deployment passes in through the downward API.
func resolveStatusSettings(
	ctx context.Context, given statusSettings, namespace, serviceAccount string, get rawGetter,
) (statusSettings, error) {
	settings := given
	if settings.Subject == "" {
		if namespace == "" || serviceAccount == "" {
			return statusSettings{}, errors.New("the operator's service account is not known: " +
				"set POD_NAMESPACE and POD_SERVICE_ACCOUNT, or pass --status-subject")
		}
		settings.Subject = serviceAccountPrefix + namespace + ":" + serviceAccount
	}
	if !agentconfig.IsServiceAccount(settings.Subject) {
		return statusSettings{}, fmt.Errorf("the subject %q is not a service account, "+
			"system:serviceaccount:<namespace>:<name>", settings.Subject)
	}
	if settings.Issuer == "" {
		ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
		defer cancel()
		issuer, err := discoverIssuer(ctx, get)
		if err != nil {
			return statusSettings{}, fmt.Errorf("the issuer of the cluster's tokens is not known "+
				"(pass --status-issuer): %w", err)
		}
		settings.Issuer = issuer
	}
	return settings, nil
}

// apiGetter returns a reader of paths of the API server that the operator is
// connected to.
func apiGetter(config *rest.Config) (rawGetter, error) {
	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create a client for the API server: %w", err)
	}
	return func(ctx context.Context, path string) ([]byte, error) {
		return client.RESTClient().Get().AbsPath(path).DoRaw(ctx)
	}, nil
}

// discoveredStatusSettings is resolveStatusSettings for the operator process: the
// service account comes from its environment and the issuer from its cluster.
func discoveredStatusSettings(get rawGetter, given statusSettings) (statusSettings, error) {
	return resolveStatusSettings(context.Background(), given,
		os.Getenv("POD_NAMESPACE"), os.Getenv("POD_SERVICE_ACCOUNT"), get)
}

// serviceAccountOf returns the namespace and the name of the service account that a
// subject names, system:serviceaccount:<namespace>:<name>. The operator asks for its
// tokens for the account that the agents accept, so the two are one fact.
func serviceAccountOf(subject string) (namespace, name string, err error) {
	remainder, ok := strings.CutPrefix(subject, serviceAccountPrefix)
	namespace, name, found := strings.Cut(remainder, ":")
	if !ok || !found || namespace == "" || name == "" || strings.Contains(name, ":") {
		return "", "", fmt.Errorf("the subject %q is not a service account, "+
			"system:serviceaccount:<namespace>:<name>", subject)
	}
	return namespace, name, nil
}

// discoverIssuer reads the issuer from the API server's discovery document.
func discoverIssuer(ctx context.Context, get rawGetter) (string, error) {
	body, err := get(ctx, discoveryPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", discoveryPath, err)
	}
	var document struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return "", fmt.Errorf("%s is not a discovery document: %w", discoveryPath, err)
	}
	if document.Issuer == "" {
		return "", fmt.Errorf("%s names no issuer", discoveryPath)
	}
	return document.Issuer, nil
}
