package app

import (
    "strconv"
    "sync"
    "sync/atomic"
)

type responseArtifacts struct {
    Thoughts    []string
    Sources     []sourceRef
    CodeSnippets []codeSnippet
}

type sourceRef struct {
    Title string
    URI   string
}

type codeSnippet struct {
    Language string
    Code     string
    Outcome  string
    Output   string
}

type artifactStore struct {
    mu      sync.RWMutex
    items   map[string]*responseArtifacts
    counter uint64
}

func newArtifactStore() *artifactStore {
    return &artifactStore{
        items: make(map[string]*responseArtifacts),
    }
}

func (s *artifactStore) put(art *responseArtifacts) string {
    if art == nil {
        return ""
    }
    id := atomic.AddUint64(&s.counter, 1)
    key := strconv.FormatUint(id, 10)
    s.mu.Lock()
    s.items[key] = art
    s.mu.Unlock()
    return key
}

func (s *artifactStore) get(id string) (*responseArtifacts, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    art, ok := s.items[id]
    return art, ok
}
