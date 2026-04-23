package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// State tracks processed UIDs so repeated runs never reclassify a message
// even if it's still in the inbox (e.g. user kept it there on purpose).
type State struct {
	path string
	mu   sync.Mutex
	data struct {
		Processed map[uint32]int64 `json:"processed"` // uid -> unix timestamp
	}
}

func LoadState(path string) (*State, error) {
	s := &State{path: path}
	s.data.Processed = map[uint32]int64{}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	if s.data.Processed == nil {
		s.data.Processed = map[uint32]int64{}
	}
	return s, nil
}

func (s *State) MarkProcessed(uid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Processed[uid] = time.Now().Unix()
}

func (s *State) IsProcessed(uid uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data.Processed[uid]
	return ok
}

func (s *State) FilterUnprocessed(uids []uint32) []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint32, 0, len(uids))
	for _, u := range uids {
		if _, done := s.data.Processed[u]; !done {
			out = append(out, u)
		}
	}
	return out
}

func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// AuditEntry is a single line in the audit log — one per attempted action.
// The log is append-only JSONL so it's both machine-readable and grep-friendly.
type AuditEntry struct {
	Time        time.Time `json:"time"`
	UID         uint32    `json:"uid"`
	Subject     string    `json:"subject"`
	From        string    `json:"from"`
	Action      string    `json:"action"`
	FromMailbox string    `json:"from_mailbox"`
	ToMailbox   string    `json:"to_mailbox"`
	Category    string    `json:"category,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Reasoning   string    `json:"reasoning,omitempty"`
	Confidence  float64   `json:"confidence,omitempty"`
}

type Audit struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

func OpenAudit(path string) (*Audit, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Audit{path: path, f: f}, nil
}

func (a *Audit) Append(e AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = a.f.Write(append(b, '\n'))
}

func (a *Audit) Close() error {
	if a == nil || a.f == nil {
		return nil
	}
	return a.f.Close()
}

// ReadLast returns the last n audit entries in chronological order.
func ReadAuditTail(path string, n int) ([]AuditEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	var all []AuditEntry
	for sc.Scan() {
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err == nil {
			all = append(all, e)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n <= 0 || n >= len(all) {
		return all, nil
	}
	return all[len(all)-n:], nil
}
