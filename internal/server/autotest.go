package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/ababank/mobile-device-agent/internal/autotest"
	"github.com/ababank/mobile-device-agent/internal/protocol"
)

// maskedValue stands in for variable values in responses; sending it back in
// an update keeps the stored value.
const maskedValue = "******"

func (s *Server) autotestRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/tests", s.handleListTests)
	api.HandleFunc("POST /api/tests", s.handleCreateTest)
	api.HandleFunc("GET /api/tests/{id}", s.handleGetTest)
	api.HandleFunc("PUT /api/tests/{id}", s.handleUpdateTest)
	api.HandleFunc("DELETE /api/tests/{id}", s.handleDeleteTest)
	api.HandleFunc("POST /api/tests/{id}/explore", s.handleStartRun(autotest.ModeExplore))
	api.HandleFunc("POST /api/tests/{id}/run", s.handleStartRun(autotest.ModeReplay))
	api.HandleFunc("GET /api/runs", s.handleListRuns)
	api.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	api.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)
	api.HandleFunc("POST /api/runs/{id}/save-steps", s.handleSaveSteps)
	api.HandleFunc("GET /api/runs/{id}/files/{name}", s.handleRunFile)
}

func masked(t *autotest.Test) *autotest.Test {
	c := *t
	if len(t.Variables) > 0 {
		c.Variables = make(map[string]string, len(t.Variables))
		for k := range t.Variables {
			c.Variables[k] = maskedValue
		}
	}
	return &c
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v); err != nil {
		return protocol.Errorf(protocol.CodeBadRequest, "invalid JSON: %v", err)
	}
	return nil
}

func (s *Server) handleListTests(w http.ResponseWriter, r *http.Request) {
	tests, err := s.AutoTest.Store.Tests()
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]*autotest.Test, len(tests))
	for i := range tests {
		out[i] = masked(&tests[i])
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateTest(w http.ResponseWriter, r *http.Request) {
	var t autotest.Test
	if err := decode(r, &t); err != nil {
		writeErr(w, err)
		return
	}
	out, err := s.AutoTest.CreateTest(&t)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, masked(out))
}

func (s *Server) handleGetTest(w http.ResponseWriter, r *http.Request) {
	t, err := s.AutoTest.Store.GetTest(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, masked(t))
}

// handleUpdateTest replaces the editable fields. Omitting "steps" keeps the
// recorded steps; a variable sent as "******" keeps its stored value.
func (s *Server) handleUpdateTest(w http.ResponseWriter, r *http.Request) {
	old, err := s.AutoTest.Store.GetTest(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var t autotest.Test
	if err := decode(r, &t); err != nil {
		writeErr(w, err)
		return
	}
	t.ID = old.ID
	for k, v := range t.Variables {
		if v == maskedValue {
			t.Variables[k] = old.Variables[k]
		}
	}
	out, err := s.AutoTest.UpdateTest(&t)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, masked(out))
}

func (s *Server) handleDeleteTest(w http.ResponseWriter, r *http.Request) {
	if err := s.AutoTest.Store.DeleteTest(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStartRun starts an exploration or replay:
// {"agentId": "...", "serial": "...", "verify": true}.
func (s *Server) handleStartRun(mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AgentID string `json:"agentId"`
			Serial  string `json:"serial"`
			Verify  *bool  `json:"verify"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if req.AgentID == "" || req.Serial == "" {
			writeErr(w, protocol.Errorf(protocol.CodeBadRequest, "agentId and serial are required"))
			return
		}
		var run *autotest.Run
		var err error
		if mode == autotest.ModeExplore {
			run, err = s.AutoTest.StartExplore(r.PathValue("id"), req.AgentID, req.Serial, req.Verify == nil || *req.Verify)
		} else {
			run, err = s.AutoTest.StartReplay(r.PathValue("id"), req.AgentID, req.Serial)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, run)
	}
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.AutoTest.Store.Runs(r.URL.Query().Get("testId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.AutoTest.GetRun(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if err := s.AutoTest.Cancel(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	s.handleGetRun(w, r)
}

func (s *Server) handleSaveSteps(w http.ResponseWriter, r *http.Request) {
	t, err := s.AutoTest.SaveSteps(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, masked(t))
}

func (s *Server) handleRunFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.AutoTest.Store.RunFile(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, p)
}
