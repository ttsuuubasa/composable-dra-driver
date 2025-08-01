/*
Copyright 2025 The CoHDI Authors.

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

package client

import (
	"cdi_dra/pkg/config"
	"cdi_dra/pkg/kube_utils"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	secretKey               = "composable-dra/composable-dra-secret"
	secretAccessInfoLength  = 1000  // 1 kB
	secretCertificateLength = 10000 // 10 kB
)

type cachedIMTokenSource struct {
	newIMTokenSource oauth2.TokenSource
	mu               sync.Mutex
	marginTime       time.Duration
	token            *oauth2.Token
}

type accessToken struct {
	Expiry int64 `json:"exp"`
}

type idManagerSecret struct {
	username      string
	password      string
	realm         string
	client_id     string
	client_secret string
}

func CachedIMTokenSource(client *CDIClient, controllers *kube_utils.KubeControllers) oauth2.TokenSource {
	return &cachedIMTokenSource{
		newIMTokenSource: &idManagerTokenSource{
			cdiclient:       client,
			kubecontrollers: controllers,
		},
		marginTime: 30 * time.Second,
	}

}

func (ts *cachedIMTokenSource) Token() (*oauth2.Token, error) {
	var token *oauth2.Token
	now := time.Now()
	ts.mu.Lock()
	token = ts.token
	ts.mu.Unlock()
	if token != nil && token.Expiry.Add(-ts.marginTime).After(now) {
		slog.Debug("Token executed: using cached token")
		return token, nil
	}
	slog.Debug("Token executed: trying to issue new token")
	token, err := ts.newIMTokenSource.Token()
	if err != nil {
		if ts.token == nil {
			slog.Error("failed to issue new token")
			return nil, err
		}
		slog.Error("unable to rotate token", "error", err)
		return ts.token, nil
	}
	slog.Info("new token is successfully issued")
	ts.token = token
	return token, nil
}

type idManagerTokenSource struct {
	cdiclient       *CDIClient
	kubecontrollers *kube_utils.KubeControllers
}

func (ts *idManagerTokenSource) Token() (*oauth2.Token, error) {
	var token oauth2.Token
	secret, err := ts.getIdManagerSecret()
	if err != nil {
		return nil, err
	}
	ctx := context.WithValue(context.Background(), RequestIDKey{}, config.RandomString(6))
	slog.Debug("trying API to get IM token", "requestID", GetRequestIdFromContext(ctx))
	imToken, err := ts.cdiclient.GetIMToken(ctx, secret)
	if err != nil {
		slog.Error("IM token API failed", "requestID", GetRequestIdFromContext(ctx))
		return nil, err
	}
	slog.Debug("IM token API completed successfully", "requestID", GetRequestIdFromContext(ctx))

	token.AccessToken = imToken.AccessToken

	parts := strings.Split(imToken.AccessToken, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid access token: %s", imToken.AccessToken)
	}

	decodedBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %s", err)
	}

	var result accessToken
	err = json.Unmarshal(decodedBytes, &result)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %s", err)
	}

	token.Expiry = time.Unix(result.Expiry, 0)

	return &token, nil
}

func (ts *idManagerTokenSource) getIdManagerSecret() (idManagerSecret, error) {
	var imSecret idManagerSecret
	secret, err := ts.kubecontrollers.GetSecret(secretKey)
	if err != nil {
		return imSecret, err
	}
	if secret != nil {
		if secret.Data != nil {
			username := string(secret.Data["username"])
			if len(username) < secretAccessInfoLength {
				imSecret.username = username
			} else {
				return imSecret, fmt.Errorf("username length exceeds the limitation")
			}

			password := string(secret.Data["password"])
			if len(password) < secretAccessInfoLength {
				imSecret.password = password
			} else {
				return imSecret, fmt.Errorf("password length exceeds the limitation")
			}

			realm := string(secret.Data["realm"])
			if len(realm) < secretAccessInfoLength {
				imSecret.realm = realm
			} else {
				return imSecret, fmt.Errorf("realm length exceeds the limitation")
			}

			client_id := string(secret.Data["client_id"])
			if len(client_id) < secretAccessInfoLength {
				imSecret.client_id = client_id
			} else {
				return imSecret, fmt.Errorf("client_id length exceeds the limitation")
			}

			client_secret := string(secret.Data["client_secret"])
			if len(client_secret) < secretAccessInfoLength {
				imSecret.client_secret = client_secret
			} else {
				return imSecret, fmt.Errorf("client_secret length exceeds the limitation")
			}
		}
	}
	return imSecret, nil
}
