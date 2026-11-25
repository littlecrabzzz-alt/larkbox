package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/simclock"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	snapshotVersion  = 1
	maxSnapshotBytes = 512 << 20
)

type checkpoint struct {
	Version int                  `json:"version"`
	Clock   *time.Time           `json:"manual_clock,omitempty"`
	State   json.RawMessage      `json:"state"`
	Retries []event.PendingRetry `json:"pending_retries"`
}

type decodedCheckpoint struct {
	form  checkpoint
	state *store.Snapshot
}

func (s *Server) snapshotBytes() ([]byte, error) {
	s.controlMu.RLock()
	defer s.controlMu.RUnlock()
	var snapshot []byte
	err := s.events.ExportCheckpoint(func(retries []event.PendingRetry) error {
		state, err := s.store.ExportSnapshot()
		if err != nil {
			return err
		}
		form := checkpoint{Version: snapshotVersion, State: state, Retries: retries}
		if s.clock != nil {
			now := s.clock.Now()
			form.Clock = &now
		}
		snapshot, err = json.Marshal(form)
		return err
	})
	if err == nil && len(snapshot) > maxSnapshotBytes {
		return nil, errors.New("snapshot exceeds the 512 MiB size limit")
	}
	return snapshot, err
}

func (s *Server) decodeCheckpoint(reader io.Reader) (decodedCheckpoint, error) {
	var result decodedCheckpoint
	limited := &io.LimitedReader{R: reader, N: maxSnapshotBytes + 1}
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if dec.Decode(&result.form) != nil || !errors.Is(dec.Decode(new(json.RawMessage)), io.EOF) || limited.N == 0 {
		return decodedCheckpoint{}, errors.New("invalid snapshot document")
	}
	if result.form.Version != snapshotVersion {
		return decodedCheckpoint{}, errors.New("unsupported snapshot version")
	}
	if (s.clock != nil) != (result.form.Clock != nil) ||
		(result.form.Clock != nil && !simclock.ValidTime(*result.form.Clock)) {
		return decodedCheckpoint{}, errors.New("snapshot clock mode does not match the server")
	}
	state, err := store.DecodeSnapshot(result.form.State)
	if err != nil {
		return decodedCheckpoint{}, err
	}
	if err := event.ValidatePending(result.form.Retries); err != nil {
		return decodedCheckpoint{}, err
	}
	result.state = state
	return result, nil
}

func (s *Server) applyCheckpoint(snapshot decodedCheckpoint) {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	s.events.RestoreCheckpoint(func() {
		s.faults.Reset()
		if s.clock != nil {
			s.clock.Restore(*snapshot.form.Clock)
		}
		s.store.RestoreSnapshot(snapshot.state)
	}, snapshot.form.Retries)
}

// LoadCheckpoint restores state before the listener starts serving requests.
// A new state file receives the configured seed.
func (s *Server) LoadCheckpoint() error {
	if s.stateFile == "" {
		return nil
	}
	// #nosec G304 -- The operator configures this local state file.
	file, err := os.Open(s.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return s.SaveCheckpoint()
	}
	if err != nil {
		return fmt.Errorf("open state file: %w", err)
	}
	defer func() { _ = file.Close() }()
	checkpoint, err := s.decodeCheckpoint(file)
	if err != nil {
		return fmt.Errorf("restore state file: %w", err)
	}
	s.applyCheckpoint(checkpoint)
	return nil
}

// SaveCheckpoint atomically replaces the configured state file.
func (s *Server) SaveCheckpoint() error {
	if s.stateFile == "" {
		return nil
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	raw, err := s.snapshotBytes()
	if err != nil {
		return err
	}
	return writeCheckpoint(s.stateFile, raw)
}

// CheckpointUntil writes the current state once per second until cancellation.
func (s *Server) CheckpointUntil(ctx context.Context, report func(error)) {
	if s.stateFile == "" {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.SaveCheckpoint(); err != nil {
				report(err)
			}
		}
	}
}

func writeCheckpoint(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".larkbox-state-*")
	if err != nil {
		return fmt.Errorf("create state checkpoint: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, writeErr := io.Copy(tmp, bytes.NewReader(body))
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write state checkpoint: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

func (s *Server) registerSnapshotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /_sandbox/snapshot", s.exportSnapshot)
	mux.HandleFunc("POST /_sandbox/snapshot", s.importSnapshot)
}

func (s *Server) exportSnapshot(w http.ResponseWriter, _ *http.Request) {
	raw, err := s.snapshotBytes()
	if err != nil {
		snapshotError(w, http.StatusInternalServerError, "could not export snapshot")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="larkbox-snapshot.json"`)
	_, _ = w.Write(raw)
}

func (s *Server) importSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.decodeCheckpoint(http.MaxBytesReader(w, r.Body, maxSnapshotBytes))
	if err != nil {
		snapshotError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if s.stateFile != "" {
		raw, err := json.Marshal(snapshot.form)
		if err != nil || writeCheckpoint(s.stateFile, raw) != nil {
			snapshotError(w, http.StatusInternalServerError, "could not persist imported snapshot")
			return
		}
	}
	s.applyCheckpoint(snapshot)
	_ = protocol.WriteJSON(w, http.StatusOK, map[string]bool{"restored": true})
}

func snapshotError(w http.ResponseWriter, status int, message string) {
	_ = protocol.WriteJSON(w, status, map[string]string{"error": message})
}
