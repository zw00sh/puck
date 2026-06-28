package server

import (
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strconv"

	"github.com/zw00sh/puck/puckview/internal/store"
)

var macRe = regexp.MustCompile(`^([0-9a-fA-F]{2}[:\-]){5}[0-9a-fA-F]{2}$`)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func validIP(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

// GET /api/devices
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	byMAC, _ := neighSnapshot()
	writeJSON(w, s.buildDevices(byMAC))
}

// POST /api/devices  {mac, ip?, name?, manual?}
func (s *Server) handleTrackDevice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MAC, IP, Name string
		Manual        bool `json:"manual"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !macRe.MatchString(in.MAC) {
		writeErr(w, http.StatusBadRequest, "invalid mac")
		return
	}
	if in.IP != "" && !validIP(in.IP) {
		writeErr(w, http.StatusBadRequest, "invalid ip")
		return
	}
	if err := s.store.Add(store.Device{MAC: in.MAC, IP: in.IP, Name: in.Name}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	mac := store.NormMAC(in.MAC)
	s.store.DeleteObserved(mac) // it's tracked now; drop it from recently-seen
	// A user-typed name is durable: mark it manual so the background resolve fills
	// only the vendor and never overwrites the name.
	if in.Manual && in.Name != "" {
		src := "manual"
		s.store.Patch(mac, store.Patch{NameSource: &src})
	}
	// Best-effort name/vendor resolution in the background (user-initiated).
	if in.IP != "" {
		go s.resolveAsync(mac, in.IP)
	}
	s.pushPresence()
	w.WriteHeader(http.StatusCreated)
}

// PATCH /api/devices/{mac}  {name?, link?, notes?, ip?}
func (s *Server) handlePatchDevice(w http.ResponseWriter, r *http.Request) {
	mac := r.PathValue("mac")
	var in struct {
		Name  *string `json:"name"`
		Link  *string `json:"link"`
		Notes *string `json:"notes"`
		IP    *string `json:"ip"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	src := "manual"
	p := store.Patch{Name: in.Name, Link: in.Link, Notes: in.Notes, IP: in.IP}
	if in.Name != nil {
		p.NameSource = &src
	}
	if err := s.store.Patch(mac, p); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.pushPresence()
	w.WriteHeader(http.StatusOK)
}

// DELETE /api/devices/{mac}
func (s *Server) handleUntrackDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Delete(r.PathValue("mac")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.pushPresence()
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/devices/{mac}/probes  {port}
func (s *Server) handleAddProbe(w http.ResponseWriter, r *http.Request) {
	mac := r.PathValue("mac")
	var in struct {
		Port int `json:"port"`
	}
	if err := decode(r, &in); err != nil || in.Port < 1 || in.Port > 65535 {
		writeErr(w, http.StatusBadRequest, "invalid port")
		return
	}
	if err := s.store.AddProbe(mac, in.Port); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.pushPresence()
	w.WriteHeader(http.StatusCreated)
}

// DELETE /api/devices/{mac}/probes/{port}
func (s *Server) handleDeleteProbe(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid port")
		return
	}
	if err := s.store.DeleteProbe(r.PathValue("mac"), port); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.pushPresence()
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/cache
func (s *Server) handleCache(w http.ResponseWriter, r *http.Request) {
	_, entries := neighSnapshot()
	writeJSON(w, s.buildCache(entries))
}
