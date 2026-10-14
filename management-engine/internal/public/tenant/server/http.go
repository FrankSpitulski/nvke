// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/limits"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

var errJSON = errors.New("invalid JSON request")

// jsonMarshaler makes the gateway validate the entire body. Its standard stream
// decoder accepts a valid first object followed by another JSON value.
type jsonMarshaler struct{ runtime.JSONPb }

func (m *jsonMarshaler) NewDecoder(reader io.Reader) runtime.Decoder {
	return runtime.DecoderFunc(func(value any) error {
		message, ok := value.(proto.Message)
		if !ok {
			return errJSON
		}
		body, err := io.ReadAll(reader)
		if err != nil || m.UnmarshalOptions.Unmarshal(body, message) != nil {
			return errJSON
		}
		return nil
	})
}

type httpRoute struct {
	clusterv1.HTTPRoute
	segments []string
	input    protoreflect.MessageDescriptor
}

func boundary(next http.Handler) (http.Handler, error) {
	routes := make([]httpRoute, 0, len(clusterv1.HTTPRoutes))
	for _, route := range clusterv1.HTTPRoutes {
		methodName := strings.ReplaceAll(strings.TrimPrefix(route.FullMethod, "/"), "/", ".")
		descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(methodName))
		if err != nil {
			return nil, fmt.Errorf("resolve HTTP route %s: %w", route.Path, err)
		}
		method, ok := descriptor.(protoreflect.MethodDescriptor)
		if !ok {
			return nil, fmt.Errorf("HTTP route %s does not refer to an RPC method", route.Path)
		}
		routes = append(routes, httpRoute{HTTPRoute: route, segments: strings.Split(route.Path, "/"), input: method.Input()})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Header[http.CanonicalHeaderKey("X-HTTP-Method-Override")]; present || r.URL.RawPath != "" {
			writeError(w, http.StatusBadRequest, codes.InvalidArgument, "request override or encoded path is unsupported")
			return
		}
		matchedPath := false
		for _, route := range routes {
			bindings, matches := route.bind(r.URL.Path)
			if !matches {
				continue
			}
			matchedPath = true
			if route.Method != r.Method {
				continue
			}
			if validateRequest(w, r, route, bindings) {
				next.ServeHTTP(w, r)
			}
			return
		}
		if matchedPath {
			writeError(w, http.StatusMethodNotAllowed, codes.Unimplemented, "method is unsupported")
		} else {
			writeError(w, http.StatusNotFound, codes.NotFound, "route not found")
		}
	}), nil
}

func (route httpRoute) bind(path string) (map[string]string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != len(route.segments) {
		return nil, false
	}
	bindings := make(map[string]string)
	for i, segment := range route.segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") && parts[i] != "" {
			bindings[strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")] = parts[i]
		} else if segment != parts[i] {
			return nil, false
		}
	}
	return bindings, true
}

func validateRequest(w http.ResponseWriter, r *http.Request, route httpRoute, bindings map[string]string) bool {
	if !validQuery(r.URL.RawQuery, route, bindings) {
		writeError(w, http.StatusBadRequest, codes.InvalidArgument, "unsupported or duplicate query parameter")
		return false
	}
	if r.Header.Get("Content-Encoding") != "" {
		writeError(w, http.StatusUnsupportedMediaType, codes.InvalidArgument, "request compression is unsupported")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		var networkError net.Error
		switch {
		case errors.As(err, &tooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, codes.ResourceExhausted, "request body exceeds limit")
		case errors.As(err, &networkError) && networkError.Timeout():
			writeError(w, http.StatusRequestTimeout, codes.DeadlineExceeded, "request body read timed out")
		default:
			writeError(w, http.StatusBadRequest, codes.InvalidArgument, "request body could not be read")
		}
		return false
	}
	if route.Body == "" {
		if len(body) != 0 {
			writeError(w, http.StatusBadRequest, codes.InvalidArgument, "request must not have a body")
			return false
		}
	} else {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, codes.InvalidArgument, "request body must be application/json")
			return false
		}
		if err := checkJSONValues(body, bindings); err != nil {
			writeError(w, http.StatusBadRequest, codes.InvalidArgument, "invalid JSON body or conflicting path identity")
			return false
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return true
}

func validQuery(raw string, route httpRoute, bindings map[string]string) bool {
	values, err := url.ParseQuery(raw)
	if err != nil || (route.Body != "" && raw != "") {
		return false
	}
	for name, entries := range values {
		field := route.input.Fields().ByJSONName(name)
		_, bound := bindings[name]
		if field == nil || field.JSONName() != name || bound || len(entries) != 1 {
			return false
		}
	}
	return true
}

// checkJSONValues enforces the two restrictions protobuf JSON does not provide:
// explicit nulls are forbidden, and body fields must agree with path bindings.
// Protobuf's decoder owns field names, types, duplicates and complete syntax.
func checkJSONValues(body []byte, bindings map[string]string) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	depth := 0
	key := ""
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil || token == nil {
			return errJSON
		}
		if delimiter, ok := token.(json.Delim); ok {
			if delimiter == '{' || delimiter == '[' {
				depth++
			} else {
				depth--
			}
			key = ""
		} else if depth == 1 {
			if key == "" {
				key, _ = token.(string)
			} else {
				if expected, bound := bindings[key]; bound && token != expected {
					return errJSON
				}
				key = ""
			}
		}
	}
}

func writeError(w http.ResponseWriter, httpCode int, code codes.Code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(httpCode)
	_ = json.NewEncoder(w).Encode(struct {
		Code    codes.Code `json:"code"`
		Message string     `json:"message"`
	}{Code: code, Message: message})
}
