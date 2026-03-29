package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

type createStreamRequest struct {
	ID            string `json:"id"`
	ListenPort    int    `json:"listen_port"`
	Upstream      string `json:"upstream"`
	ContainerPort int    `json:"container_port,omitempty"`
	Protocol      string `json:"protocol"`
	Domain        string `json:"domain,omitempty"`
}

func (s *Server) handleStreams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		streams, err := s.Store.ListStreams()
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, streams)
	case http.MethodPost:
		var req createStreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid json")
			return
		}
		slog.Info("stream_create_requested",
			"request_id", requestIDFromContext(r.Context()),
			"stream_id", strings.TrimSpace(req.ID),
			"listen_port", req.ListenPort,
			"upstream", req.Upstream,
			"protocol", req.Protocol,
			"domain", req.Domain,
		)

		normalizedUpstream, err := normalizeStreamUpstream(req.Upstream, req.ContainerPort)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		mappedUpstream, containerName, containerNetwork, err := s.mapEndpointToContainerIP(normalizedUpstream, 0, "")
		if err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}

		stream := models.Stream{
			ID:               strings.TrimSpace(req.ID),
			ListenPort:       req.ListenPort,
			Upstream:         mappedUpstream,
			ContainerName:    containerName,
			ContainerNetwork: containerNetwork,
			Protocol:         strings.ToLower(strings.TrimSpace(req.Protocol)),
			Domain:           strings.TrimSpace(req.Domain),
		}

		if stream.ListenPort == 0 {
			streams, err := s.Store.ListStreams()
			if err != nil {
				errorResponse(w, http.StatusInternalServerError, "failed to list streams: "+err.Error())
				return
			}

			usedPorts := make(map[int]bool)
			for _, str := range streams {
				usedPorts[str.ListenPort] = true
			}

			var candidates []int
			for p := 30000; p <= 30100; p++ {
				if !usedPorts[p] {
					candidates = append(candidates, p)
				}
			}

			if len(candidates) == 0 {
				errorResponse(w, http.StatusInternalServerError, "no available ports in range 30000-30100")
				return
			}
			stream.ListenPort = candidates[rand.Intn(len(candidates))]
		}

		if stream.ID == "" {
			stream.ID = fmt.Sprintf("stream-%d", stream.ListenPort)
		}
		if stream.Protocol == "" {
			stream.Protocol = "tcp"
		}

		stream.CreatedAt = time.Now()
		stream.UpdatedAt = time.Now()
		stream.Status = "provisioning"

		if err := s.Store.SaveStream(&stream); err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.Info("stream_create_accepted",
			"request_id", requestIDFromContext(r.Context()),
			"stream_id", stream.ID,
			"listen_port", stream.ListenPort,
			"upstream", stream.Upstream,
			"container_name", stream.ContainerName,
			"container_network", stream.ContainerNetwork,
		)

		streamCopy := stream
		go s.provisionStream(&streamCopy)

		jsonResponse(w, http.StatusCreated, stream)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStreamDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/streams/"):]
	if id == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		stream, err := s.Store.GetStream(id)
		if err != nil {
			errorResponse(w, http.StatusNotFound, "stream not found")
			return
		}
		jsonResponse(w, http.StatusOK, stream)
	case http.MethodDelete:
		stream, err := s.Store.GetStream(id)
		if err != nil {
			errorResponse(w, http.StatusNotFound, "stream not found")
			return
		}
		port := stream.ListenPort

		if err := s.Store.DeleteStream(id); err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.Info("stream_delete_accepted",
			"request_id", requestIDFromContext(r.Context()),
			"stream_id", id,
			"listen_port", port,
		)

		go s.reconcileStreams(port)
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) reconcileStreams(port int) {
	s.reconcileStreamsWithReload(port, true)
}

func (s *Server) reconcileStreamsWithReload(port int, reload bool) {
	slog.Info("Reconciling streams", "port", port)

	allStreams, err := s.Store.ListStreams()
	if err != nil {
		slog.Error("reconcile error: failed to list streams", "error", err)
		return
	}

	var portStreams []models.Stream
	for _, str := range allStreams {
		if str.ListenPort != port {
			continue
		}
		if str.Disabled {
			s.updateStreamStatus(str.ID, "error", "tracked container missing for over 5 minutes; stream disabled to avoid stale IP routing")
			continue
		}
		if strings.TrimSpace(str.Upstream) == "" {
			s.updateStreamStatus(str.ID, "error", "empty upstream")
			continue
		}
		portStreams = append(portStreams, str)
	}
	slog.Debug("Found streams for port", "port", port, "count", len(portStreams))

	var rebuildErr error
	if reload {
		rebuildErr = s.Nginx.RebuildStreamConfig(port, portStreams)
	} else {
		rebuildErr = s.Nginx.RebuildStreamConfigNoReload(port, portStreams)
	}
	if rebuildErr != nil {
		slog.Error("reconcile error: failed to rebuild config", "port", port, "error", rebuildErr)
		return
	}

	for _, str := range portStreams {
		if str.Status != "active" {
			s.updateStreamStatus(str.ID, "active", "")
		}
	}
	slog.Info("Stream reconciliation complete", "port", port)
}

func (s *Server) provisionStream(stream *models.Stream) {
	s.reconcileStreams(stream.ListenPort)
}

func (s *Server) updateStreamStatus(id, status, msg string) {
	stream, err := s.Store.GetStream(id)
	if err != nil {
		return
	}
	stream.Status = status
	stream.ErrorMessage = msg
	stream.UpdatedAt = time.Now()
	s.Store.SaveStream(stream)
}
