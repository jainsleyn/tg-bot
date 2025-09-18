package app

import (
    "google.golang.org/genai"
    "sync"
)

const maxHistoryEntries = 20

type thinkingMode string

const (
    thinkingModeLow     thinkingMode = "low"
    thinkingModeMedium  thinkingMode = "medium"
    thinkingModeHigh    thinkingMode = "high"
    thinkingModeDynamic thinkingMode = "dynamic"
)

func defaultThinkingMode() thinkingMode {
    return thinkingModeMedium
}

func parseThinkingMode(v string) thinkingMode {
    switch thinkingMode(v) {
    case thinkingModeLow, thinkingModeMedium, thinkingModeHigh, thinkingModeDynamic:
        return thinkingMode(v)
    default:
        return defaultThinkingMode()
    }
}

func (m thinkingMode) budgetTokens() *int32 {
    switch m {
    case thinkingModeLow:
        v := int32(4096)
        return &v
    case thinkingModeMedium:
        v := int32(16384)
        return &v
    case thinkingModeHigh:
        v := int32(32768)
        return &v
    case thinkingModeDynamic:
        v := int32(-1)
        return &v
    default:
        v := int32(16384)
        return &v
    }
}

func (m thinkingMode) label() string {
    switch m {
    case thinkingModeLow:
        return "Low - 4,096 tokens"
    case thinkingModeMedium:
        return "Medium - 16,384 tokens"
    case thinkingModeHigh:
        return "High - 32,768 tokens"
    case thinkingModeDynamic:
        return "Dynamic reasoning"
    default:
        return "Medium - 16,384 tokens"
    }
}

type sessionManager struct {
    mu          sync.RWMutex
    sessions    map[int64]*sessionState
    defaultMode thinkingMode
}

type sessionState struct {
    mu       sync.Mutex
    history  []*genai.Content
    thinking thinkingMode
}

func newSessionManager(defaultMode thinkingMode) *sessionManager {
    return &sessionManager{
        sessions:    make(map[int64]*sessionState),
        defaultMode: defaultMode,
    }
}

func (m *sessionManager) get(chatID int64) *sessionState {
    m.mu.RLock()
    session, ok := m.sessions[chatID]
    m.mu.RUnlock()
    if ok {
        return session
    }

    session = &sessionState{thinking: m.defaultMode}
    m.mu.Lock()
    m.sessions[chatID] = session
    m.mu.Unlock()
    return session
}

func (s *sessionState) conversationWith(user *genai.Content) []*genai.Content {
    convo := make([]*genai.Content, 0, len(s.history)+1)
    convo = append(convo, s.history...)
    convo = append(convo, user)
    return convo
}

func (s *sessionState) appendTurn(user *genai.Content, model *genai.Content) {
    if user != nil {
        s.history = append(s.history, user)
    }
    if model != nil {
        s.history = append(s.history, model)
    }
    if len(s.history) > maxHistoryEntries {
        s.history = append([]*genai.Content{}, s.history[len(s.history)-maxHistoryEntries:]...)
    }
}

func (s *sessionState) currentThinking() thinkingMode {
    if s.thinking == "" {
        s.thinking = defaultThinkingMode()
    }
    return s.thinking
}

func (s *sessionState) setThinking(mode thinkingMode) {
    s.thinking = mode
}
