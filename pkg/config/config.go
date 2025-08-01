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

package config

import (
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	validator "github.com/go-playground/validator/v10"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	DeviceInfoKey  = "device-info"
	LabelPrefixKey = "label-prefix"
)

type Config struct {
	LogLevel      int
	ScanInterval  time.Duration
	TenantID      string
	ClusterID     string
	CDIEndpoint   string
	UseCapiBmh    bool
	UseCM         bool
	BindingTimout *int64
}

type DeviceInfo struct {
	// Index of device
	Index int `yaml:"index" validate:"gte=0,lte=10000"`
	// Name of a device model registered to ResourceManager in CDI
	CDIModelName string `yaml:"cdi-model-name" validate:"max=1000"`
	// Attributes of ResourceSlice that will be exposed. It corresponds to vendor's ResourceSlice
	DRAAttributes map[string]string `yaml:"dra-attributes" validate:"max=100,dive,keys,max=1000,endkeys,max=1000"`
	// Name of vendor DRA driver for a device
	DriverName string `yaml:"driver-name" validate:"max=1000"`
	// DRA pool name or label name affixed to a node. Basic format is "<vendor>-<model>"
	K8sDeviceName string `yaml:"k8s-device-name" validate:"max=50,is-dns"`
	// List of device indexes unable to coexist in the same node
	CanNotCoexistWith []int `yaml:"cannot-coexists-with" validate:"max=100"`
}

func GetDeviceInfos(cm *corev1.ConfigMap) ([]DeviceInfo, error) {
	if cm.Data == nil {
		slog.Warn("configmap data is nil")
		return nil, nil
	}
	if devInfoStr, found := cm.Data[DeviceInfoKey]; !found {
		slog.Warn("configmap device-info is nil")
		return nil, nil
	} else {
		var devInfos []DeviceInfo
		bytes := []byte(devInfoStr)
		err := yaml.Unmarshal(bytes, &devInfos)
		if err != nil {
			slog.Error("Failed yaml unmarshal", "error", err)
			return nil, err
		}
		// Validate the factor in device-info
		validate := validator.New()
		validate.RegisterValidation("is-dns", ValidateDNSLabel)
		for _, devInfo := range devInfos {
			if err := validate.Struct(devInfo); err != nil {
				return nil, err
			}
		}
		return devInfos, nil
	}
}

func ValidateDNSLabel(fl validator.FieldLevel) bool {
	value := fl.Field().String()
	errs := validation.IsDNS1123Label(value)
	if len(errs) > 0 {
		for _, err := range errs {
			slog.Error("validation error. It must be DNS label", "value", value, "error", err)
		}
		return false
	} else {
		return true
	}
}

func GetLabelPrefix(cm *corev1.ConfigMap) (string, error) {
	if cm.Data == nil {
		slog.Warn("configmap data is nil")
		return "", nil
	}
	if labelPrefix, found := cm.Data[LabelPrefixKey]; !found {
		slog.Warn("configmap label-prefix is nil")
		return "", nil
	} else {
		errs := validation.IsDNS1123Subdomain(labelPrefix)
		if len(labelPrefix) > 100 {
			errs = append(errs, "label-prefix length exceeds 100B")
		}
		if len(errs) > 0 {
			for _, err := range errs {
				slog.Error("validation error for label-prefix", "error", err)
			}
			return "", fmt.Errorf("label-prefix validation error")
		}
		return labelPrefix, nil
	}
}

const CharSet = "123456789"

func RandomString(n int) string {
	result := make([]byte, n)
	for i := range result {
		result[i] = CharSet[rand.Intn(len(CharSet))]
	}
	return string(result)
}
