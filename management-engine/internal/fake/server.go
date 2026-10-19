// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package fake provides a seeded NVKE controller API for controller fixtures.
package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

type resourceCollection[T any] struct {
	StatusCode int `json:"statusCode,omitempty"`
	Resources  T   `json:"resources,omitempty"`
}

type requestRecord struct {
	Method string                    `json:"method"`
	Path   string                    `json:"path"`
	Create *api.CreateClusterRequest `json:"create,omitempty"`
}

type serverSeed struct {
	Clusters resourceCollection[[]api.Cluster] `json:"clusters"`
	Create   json.RawMessage                   `json:"create,omitempty"`
}

type serverDump struct {
	Clusters []api.Cluster   `json:"clusters"`
	Requests []requestRecord `json:"requests"`
}

// Server owns one fixture's external records and write requests.
type Server struct {
	mu       sync.Mutex
	clusters resourceCollection[map[uuid.UUID]api.Cluster]
	create   json.RawMessage
	requests []requestRecord
}

// New returns an empty fake API with no lifecycle simulation.
func New() *Server {
	return &Server{clusters: resourceCollection[map[uuid.UUID]api.Cluster]{Resources: map[uuid.UUID]api.Cluster{}}}
}

// Handler returns the HTTP surface used by controller envtests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/clusters", handle(s, &s.clusters, s.listClusters))
	mux.HandleFunc("GET /api/clusters/{clusterID}", handle(s, &s.clusters, s.getCluster))
	mux.HandleFunc("POST /api/clusters", handle(s, &s.clusters, s.createCluster))
	mux.HandleFunc("DELETE /api/clusters/{clusterID}", handle(s, &s.clusters, s.deleteCluster))
	return mux
}

func handle[T any](s *Server, collection *resourceCollection[T], handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		status := collection.StatusCode
		s.mu.Unlock()
		if status != 0 {
			writeError(w, status, http.StatusText(status))
			return
		}
		handler(w, r)
	}
}

// SeedFromYAML loads existing records and the declared defaults for newly created records.
func (s *Server) SeedFromYAML(input string) error {
	var seed serverSeed
	if err := yaml.UnmarshalStrict([]byte(input), &seed); err != nil {
		return fmt.Errorf("decode fake NVKE controller resources: %w", err)
	}
	if seed.Clusters.StatusCode != 0 && (seed.Clusters.StatusCode < 400 || seed.Clusters.StatusCode > 599) {
		return fmt.Errorf("clusters.statusCode must be an HTTP error status or zero")
	}
	for _, cluster := range seed.Clusters.Resources {
		if cluster.ID == uuid.Nil || cluster.Name == "" {
			return fmt.Errorf("seeded cluster requires a nonzero id and name")
		}
	}
	if len(seed.Create) != 0 {
		var cluster api.Cluster
		if err := json.Unmarshal(seed.Create, &cluster); err != nil {
			return fmt.Errorf("decode create defaults: %w", err)
		}
		if cluster.ID == uuid.Nil {
			return fmt.Errorf("create defaults require a nonzero id")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusters.StatusCode = seed.Clusters.StatusCode
	for _, cluster := range seed.Clusters.Resources {
		s.clusters.Resources[cluster.ID] = cluster
	}
	s.create = seed.Create
	return nil
}

// Dump snapshots stored records and deduplicated writes without counting read polling.
func (s *Server) Dump() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dump := serverDump{Clusters: s.sortedClusters(), Requests: []requestRecord{}}
	for _, request := range s.requests {
		if !slices.ContainsFunc(dump.Requests, func(existing requestRecord) bool {
			return reflect.DeepEqual(existing, request)
		}) {
			dump.Requests = append(dump.Requests, request)
		}
	}
	content, err := yaml.Marshal(dump)
	if err != nil {
		return "", fmt.Errorf("marshal fake NVKE controller state: %w", err)
	}
	return string(content), nil
}

func (s *Server) sortedClusters() []api.Cluster {
	clusters := make([]api.Cluster, 0, len(s.clusters.Resources))
	for _, cluster := range s.clusters.Resources {
		clusters = append(clusters, cluster)
	}
	slices.SortFunc(clusters, func(a, b api.Cluster) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return clusters
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clusters := s.sortedClusters()
	if name := r.URL.Query().Get("name"); name != "" {
		clusters = slices.DeleteFunc(clusters, func(cluster api.Cluster) bool { return cluster.Name != name })
	}
	writeJSON(w, http.StatusOK, api.ListClustersResponse{Clusters: clusters})
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := uuid.Parse(r.PathValue("clusterID"))
	cluster, exists := s.clusters.Resources[id]
	if err != nil || !exists {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	writeJSON(w, http.StatusOK, cluster)
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	var request api.CreateClusterRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid create request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, requestRecord{Method: r.Method, Path: r.URL.Path, Create: &request})
	for _, cluster := range s.clusters.Resources {
		if cluster.Name == request.Name {
			writeError(w, http.StatusBadRequest, "cluster name already exists")
			return
		}
	}
	var cluster api.Cluster
	if err := json.Unmarshal(s.create, &cluster); err != nil {
		writeError(w, http.StatusInternalServerError, "fixture has no valid create defaults")
		return
	}
	if _, exists := s.clusters.Resources[cluster.ID]; exists {
		writeError(w, http.StatusConflict, "create defaults reuse an existing cluster ID")
		return
	}
	// Request fields update the record while conditions remain exactly as declared in the seed.
	body, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot encode create request")
		return
	}
	if err := json.Unmarshal(body, &cluster); err != nil {
		writeError(w, http.StatusBadRequest, "cannot apply create request")
		return
	}
	s.clusters.Resources[cluster.ID] = cluster
	writeJSON(w, http.StatusCreated, cluster)
}

func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, requestRecord{Method: r.Method, Path: r.URL.Path})
	id, err := uuid.Parse(r.PathValue("clusterID"))
	if _, exists := s.clusters.Resources[id]; err != nil || !exists {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	delete(s.clusters.Resources, id)
	w.WriteHeader(http.StatusAccepted)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, api.GenericError{Message: message})
}
