package app

import (
    "bytes"
    "context"
    "errors"
    "fmt"
    "log"
    "mime"
    "net/http"
    "path/filepath"
    "strings"
    "time"

    tele "gopkg.in/telebot.v4"
    "google.golang.org/genai"
)

const (
    geminiModel             = "gemini-2.5-pro"
    showThoughtsUnique      = "show_thoughts"
    showSourcesUnique       = "show_sources"
    showCodeUnique          = "show_code"
    selectThinkingModeUnique = "set_thinking_mode"
)

// Config groups startup parameters for the bot runtime.
type Config struct {
    TelegramToken string
    GeminiAPIKey  string
}

// Validate ensures the configuration includes mandatory values.
func (c Config) Validate() error {
    if strings.TrimSpace(c.TelegramToken) == "" {
        return errors.New("TELEGRAM_BOT_TOKEN is required")
    }
    if strings.TrimSpace(c.GeminiAPIKey) == "" {
        return errors.New("GEMINI_API_KEY is required")
    }
    return nil
}

// App wires Telegram updates to the Gemini client.
type App struct {
    bot               *tele.Bot
    client            *genai.Client
    sessions          *sessionManager
    artifacts         *artifactStore
    systemInstruction *genai.Content
    tools             []*genai.Tool
}

// New initialises the Telegram bot and Gemini client.
func New(ctx context.Context, cfg Config) (*App, error) {
    if err := cfg.Validate(); err != nil {
        return nil, err
    }

    client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: cfg.GeminiAPIKey})
    if err != nil {
        return nil, fmt.Errorf("create genai client: %w", err)
    }

    bot, err := tele.NewBot(tele.Settings{
        Token:   cfg.TelegramToken,
        ParseMode: tele.ModeMarkdownV2,
        Poller:  &tele.LongPoller{Timeout: 10 * time.Second},
    })
    if err != nil {
        return nil, fmt.Errorf("create telebot: %w", err)
    }

    app := &App{
        bot:               bot,
        client:            client,
        sessions:          newSessionManager(defaultThinkingMode()),
        artifacts:         newArtifactStore(),
        systemInstruction: buildSystemInstruction(),
        tools: []*genai.Tool{
            {
                GoogleSearchRetrieval: &genai.GoogleSearchRetrieval{},
                URLContext:            &genai.URLContext{},
                CodeExecution:         &genai.ToolCodeExecution{},
            },
        },
    }

    app.registerHandlers()
    return app, nil
}

// Run starts the Telegram polling loop.
func (a *App) Run(ctx context.Context) error {
    go func() {
        <-ctx.Done()
        a.bot.Stop()
    }()
    a.bot.Start()
    return nil
}

func (a *App) registerHandlers() {
    a.bot.Handle("/start", func(c tele.Context) error {
        welcome := "Hi, I am Eteon. Share a prompt, a link, or media and I will respond concisely."
        _, err := a.sendWithFallback(c.Chat(), welcome, &tele.SendOptions{DisableWebPagePreview: true})
        return err
    })

    a.bot.Handle("/settings", a.handleSettings)

    messageHandler := func(c tele.Context) error {
        return a.handleUserMessage(c)
    }

    a.bot.Handle(tele.OnText, messageHandler)
    a.bot.Handle(tele.OnPhoto, messageHandler)
    a.bot.Handle(tele.OnVideo, messageHandler)
    a.bot.Handle(tele.OnAudio, messageHandler)
    a.bot.Handle(tele.OnDocument, messageHandler)
    a.bot.Handle(tele.OnVoice, messageHandler)
    a.bot.Handle(tele.OnVideoNote, messageHandler)

    a.bot.Handle(&tele.InlineButton{Unique: showThoughtsUnique}, a.handleShowThoughts)
    a.bot.Handle(&tele.InlineButton{Unique: showSourcesUnique}, a.handleShowSources)
    a.bot.Handle(&tele.InlineButton{Unique: showCodeUnique}, a.handleShowCode)
    a.bot.Handle(&tele.InlineButton{Unique: selectThinkingModeUnique}, a.handleModeSelection)
}

func (a *App) handleSettings(c tele.Context) error {
    session := a.sessions.get(c.Chat().ID)

    menu := &tele.ReplyMarkup{}
    btnLow := menu.Data("Low - 4,096 tokens", selectThinkingModeUnique, string(thinkingModeLow))
    btnMed := menu.Data("Medium - 16,384 tokens", selectThinkingModeUnique, string(thinkingModeMedium))
    btnHigh := menu.Data("High - 32,768 tokens", selectThinkingModeUnique, string(thinkingModeHigh))
    btnDyn := menu.Data("Dynamic reasoning", selectThinkingModeUnique, string(thinkingModeDynamic))

    menu.Inline(menu.Row(btnLow))
    menu.Inline(menu.Row(btnMed))
    menu.Inline(menu.Row(btnHigh))
    menu.Inline(menu.Row(btnDyn))

    body := fmt.Sprintf("Current thinking budget: %s", session.currentThinking().label())
    _, err := a.sendWithFallback(c.Chat(), body, &tele.SendOptions{ReplyMarkup: menu, DisableWebPagePreview: true})
    return err
}

func (a *App) handleModeSelection(c tele.Context) error {
    if err := c.Respond(); err != nil {
        log.Println("callback acknowledge error:", err)
    }

    payload := c.Callback().Data
    mode := parseThinkingMode(payload)
    session := a.sessions.get(c.Chat().ID)

    session.mu.Lock()
    session.setThinking(mode)
    session.mu.Unlock()

    confirmation := fmt.Sprintf("Thinking budget switched to %s", mode.label())
    _, err := a.sendWithFallback(c.Chat(), confirmation, &tele.SendOptions{DisableWebPagePreview: true})
    return err
}

func (a *App) handleUserMessage(c tele.Context) error {
    msg := c.Message()
    if msg == nil {
        return nil
    }

    session := a.sessions.get(msg.Chat.ID)
    session.mu.Lock()
    defer session.mu.Unlock()

    parts, err := a.collectParts(msg)
    if err != nil {
        log.Println("collect parts:", err)
        _, sendErr := a.sendWithFallback(msg.Chat, "I could not process that input.", &tele.SendOptions{DisableWebPagePreview: true})
        if sendErr != nil {
            log.Println("notify failure:", sendErr)
        }
        return err
    }
    if len(parts) == 0 {
        _, err := a.sendWithFallback(msg.Chat, "Please send text or supported media.", &tele.SendOptions{DisableWebPagePreview: true})
        return err
    }

    userContent := genai.NewContentFromParts(parts, genai.RoleUser)
    conversation := session.conversationWith(userContent)
    cfg := a.buildGenerateConfig(session.currentThinking())

    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
    defer cancel()

    resp, err := a.client.Models.GenerateContent(ctx, geminiModel, conversation, cfg)
    if err != nil {
        log.Println("genai request:", err)
        _, sendErr := a.sendWithFallback(msg.Chat, "Eteon could not complete that request.", &tele.SendOptions{DisableWebPagePreview: true})
        if sendErr != nil {
            log.Println("notify failure:", sendErr)
        }
        return err
    }

    if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != genai.BlockedReasonUnspecified {
        warning := "The request was blocked by safety filters."
        _, sendErr := a.sendWithFallback(msg.Chat, warning, &tele.SendOptions{DisableWebPagePreview: true})
        if sendErr != nil {
            log.Println("notify failure:", sendErr)
        }
        return nil
    }

    reply, artifacts := a.renderResponse(resp)
    if reply == "" {
        reply = "No content received."
    }

    if candidate := firstCandidate(resp); candidate != nil && candidate.Content != nil {
        session.appendTurn(userContent, filterModelContent(candidate.Content))
    } else {
        session.appendTurn(userContent, nil)
    }

    var markup *tele.ReplyMarkup
    recordID := a.artifacts.put(artifacts)
    if recordID != "" {
        markup = a.buildResponseMarkup(recordID, artifacts)
    }

    _, sendErr := a.sendWithFallback(msg.Chat, reply, &tele.SendOptions{ReplyMarkup: markup, DisableWebPagePreview: true})
    if sendErr != nil {
        return sendErr
    }
    return nil
}

func (a *App) handleShowThoughts(c tele.Context) error {
    if err := c.Respond(); err != nil {
        log.Println("callback acknowledge error:", err)
    }
    id := c.Callback().Data
    art, ok := a.artifacts.get(id)
    prompt := "Reasoning summary is unavailable."
    if ok && len(art.Thoughts) > 0 {
        steps := summarizeThoughts(art.Thoughts, 5)
        if len(steps) > 0 {
            var b strings.Builder
            b.WriteString("Reasoning summary:\n")
            for _, step := range steps {
                b.WriteString("- ")
                b.WriteString(step)
                b.WriteString("\n")
            }
            prompt = strings.TrimRight(b.String(), "\n")
        }
    }

    placeholder, err := a.sendWithFallback(c.Chat(), "Summarising thoughts...", &tele.SendOptions{DisableWebPagePreview: true})
    if err != nil {
        return err
    }
    _, err = a.editWithFallback(placeholder, prompt, &tele.SendOptions{DisableWebPagePreview: true})
    return err
}

func (a *App) handleShowSources(c tele.Context) error {
    if err := c.Respond(); err != nil {
        log.Println("callback acknowledge error:", err)
    }
    id := c.Callback().Data
    art, ok := a.artifacts.get(id)
    if !ok || len(art.Sources) == 0 {
        _, err := a.sendWithFallback(c.Chat(), "No sources available for this reply.", &tele.SendOptions{DisableWebPagePreview: true})
        return err
    }

    var b strings.Builder
    b.WriteString("Sources:\n")
    for i, src := range art.Sources {
        title := src.Title
        if title == "" {
            title = "Untitled"
        }
        b.WriteString(fmt.Sprintf("%d. %s - %s\n", i+1, title, src.URI))
    }
    body := strings.TrimRight(b.String(), "\n")
    _, err := a.sendWithFallback(c.Chat(), body, &tele.SendOptions{DisableWebPagePreview: false})
    return err
}

func (a *App) handleShowCode(c tele.Context) error {
    if err := c.Respond(); err != nil {
        log.Println("callback acknowledge error:", err)
    }
    id := c.Callback().Data
    art, ok := a.artifacts.get(id)
    if !ok || len(art.CodeSnippets) == 0 {
        _, err := a.sendWithFallback(c.Chat(), "No executable code was used for this reply.", &tele.SendOptions{DisableWebPagePreview: true})
        return err
    }

    var sections []string
    for idx, snippet := range art.CodeSnippets {
        sections = append(sections, formatCodeSnippet(idx+1, snippet))
    }
    body := strings.Join(sections, "\n\n")
    _, err := a.sendWithFallback(c.Chat(), body, &tele.SendOptions{DisableWebPagePreview: true})
    return err
}

func (a *App) collectParts(msg *tele.Message) ([]*genai.Part, error) {
    var parts []*genai.Part

    text := strings.TrimSpace(msg.Text)
    if text != "" {
        parts = append(parts, genai.NewPartFromText(text))
    }
    if caption := strings.TrimSpace(msg.Caption); caption != "" && caption != text {
        parts = append(parts, genai.NewPartFromText(caption))
    }

    if msg.Photo != nil {
        if part, err := a.partFromFile(msg.Photo.File, msg.Photo.MIME); err == nil {
            parts = append(parts, part)
        } else {
            return nil, err
        }
    }

    if msg.Document != nil {
        if part, err := a.partFromFile(msg.Document.File, msg.Document.MIME); err == nil {
            parts = append(parts, part)
        } else {
            return nil, err
        }
    }

    if msg.Video != nil {
        if part, err := a.partFromFile(msg.Video.File, msg.Video.MIME); err == nil {
            parts = append(parts, part)
        } else {
            return nil, err
        }
    }

    if msg.Audio != nil {
        if part, err := a.partFromFile(msg.Audio.File, msg.Audio.MIME); err == nil {
            parts = append(parts, part)
        } else {
            return nil, err
        }
    }

    if msg.Voice != nil {
        if part, err := a.partFromFile(msg.Voice.File, msg.Voice.MIME); err == nil {
            parts = append(parts, part)
        } else {
            return nil, err
        }
    }

    if msg.VideoNote != nil {
        if part, err := a.partFromFile(msg.VideoNote.File, msg.VideoNote.MIME); err == nil {
            parts = append(parts, part)
        } else {
            return nil, err
        }
    }

    return parts, nil
}

func (a *App) partFromFile(file tele.File, explicitMIME string) (*genai.Part, error) {
    mediaFile := file
    if err := a.bot.File(&mediaFile); err != nil {
        return nil, fmt.Errorf("get file: %w", err)
    }

    var buf bytes.Buffer
    if err := a.bot.Download(&mediaFile, &buf); err != nil {
        return nil, fmt.Errorf("download file: %w", err)
    }

    data := buf.Bytes()
    if len(data) == 0 {
        return nil, errors.New("empty media payload")
    }

    mimeType := explicitMIME
    if mimeType == "" {
        if mediaFile.FilePath != "" {
            if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(mediaFile.FilePath))); detected != "" {
                mimeType = detected
            }
        }
        if mimeType == "" {
            mimeType = http.DetectContentType(data)
        }
    }

    return &genai.Part{InlineData: &genai.Blob{Data: data, MIMEType: mimeType}}, nil
}

func (a *App) buildGenerateConfig(mode thinkingMode) *genai.GenerateContentConfig {
    budget := mode.budgetTokens()
    thinkingConfig := &genai.ThinkingConfig{IncludeThoughts: true}
    if budget != nil {
        thinkingConfig.ThinkingBudget = budget
    }

    generationThinking := &genai.GenerationConfigThinkingConfig{IncludeThoughts: true}
    if budget != nil {
        generationThinking.ThinkingBudget = budget
    }

    return &genai.GenerateContentConfig{
        SystemInstruction: a.systemInstruction,
        Tools:             a.tools,
        ThinkingConfig:    thinkingConfig,
        GenerationConfig:  &genai.GenerationConfig{ThinkingConfig: generationThinking},
    }
}

func (a *App) renderResponse(resp *genai.GenerateContentResponse) (string, *responseArtifacts) {
    cand := firstCandidate(resp)
    if cand == nil || cand.Content == nil {
        return "", &responseArtifacts{}
    }

    var mainParts []string
    var thoughtParts []string
    var codeSnippets []codeSnippet

    for _, part := range cand.Content.Parts {
        if part == nil {
            continue
        }
        if part.Thought {
            if text := strings.TrimSpace(part.Text); text != "" {
                thoughtParts = append(thoughtParts, text)
            }
            continue
        }
        if text := strings.TrimSpace(part.Text); text != "" {
            mainParts = append(mainParts, text)
        }
        if part.CodeExecutionResult != nil {
            if out := strings.TrimSpace(part.CodeExecutionResult.Output); out != "" {
                mainParts = append(mainParts, fmt.Sprintf("Result:\n%s", out))
            }
        }
        if part.ExecutableCode != nil {
            snippet := codeSnippet{
                Language: string(part.ExecutableCode.Language),
                Code:     part.ExecutableCode.Code,
            }
            if part.CodeExecutionResult != nil {
                snippet.Outcome = string(part.CodeExecutionResult.Outcome)
                snippet.Output = part.CodeExecutionResult.Output
            }
            codeSnippets = append(codeSnippets, snippet)
        }
    }

    sources := collectSources(cand)

    reply := strings.TrimSpace(strings.Join(mainParts, "\n\n"))
    art := &responseArtifacts{
        Thoughts:    thoughtParts,
        Sources:     sources,
        CodeSnippets: codeSnippets,
    }
    return reply, art
}

func (a *App) buildResponseMarkup(id string, art *responseArtifacts) *tele.ReplyMarkup {
    if id == "" || art == nil {
        return nil
    }
    markup := &tele.ReplyMarkup{}
    thoughtBtn := markup.Data("Show thoughts", showThoughtsUnique, id)
    markup.Inline(markup.Row(thoughtBtn))

    if len(art.Sources) > 0 {
        sourcesBtn := markup.Data("Show sources", showSourcesUnique, id)
        markup.Inline(markup.Row(sourcesBtn))
    }
    if len(art.CodeSnippets) > 0 {
        codeBtn := markup.Data("Show code", showCodeUnique, id)
        markup.Inline(markup.Row(codeBtn))
    }
    return markup
}

func (a *App) sendWithFallback(recipient tele.Recipient, text string, opts *tele.SendOptions) (*tele.Message, error) {
    if opts == nil {
        opts = &tele.SendOptions{}
    }
    cloned := *opts
    if cloned.ParseMode == "" {
        cloned.ParseMode = tele.ModeMarkdownV2
    }
    msg, err := a.bot.Send(recipient, text, &cloned)
    if err == nil || !isParseError(err) {
        return msg, err
    }

    cloned = *opts
    cloned.ParseMode = tele.ModeMarkdownV2
    safe := escapeMarkdownV2(text)
    return a.bot.Send(recipient, safe, &cloned)
}

func (a *App) editWithFallback(msg *tele.Message, text string, opts *tele.SendOptions) (*tele.Message, error) {
    cloned := tele.SendOptions{}
    if opts != nil {
        cloned = *opts
    }
    if cloned.ParseMode == "" {
        cloned.ParseMode = tele.ModeMarkdownV2
    }
    edited, err := a.bot.Edit(msg, text, &cloned)
    if err == nil || !isParseError(err) {
        return edited, err
    }

    cloned.ParseMode = tele.ModeMarkdownV2
    safe := escapeMarkdownV2(text)
    return a.bot.Edit(msg, safe, &cloned)
}

func isParseError(err error) bool {
    if err == nil {
        return false
    }
    msg := err.Error()
    return strings.Contains(msg, "can't parse entities") || strings.Contains(msg, "can't parse message")
}

func buildSystemInstruction() *genai.Content {
    prompt := strings.Join([]string{
        "You are Eteon, a concise assistant powered by Gemini 2.5 Pro.",
        "Always provide focused, high-signal answers and respect the user's language.",
        "When information may be outdated or needs verification, use the available web grounding search before responding.",
        "Run calculations and data transformations through the code execution tool whenever computation is involved, and use its results in the final answer.",
        "Load any user-provided URLs via the URL context tool to ground your responses in those sources.",
        "Handle multimodal inputs such as images, audio, and video without asking the user to reformat them.",
        "Produce replies that comply with Telegram MarkdownV2 formatting rules.",
    }, " ")
    return genai.NewContentFromText(prompt, genai.Role("system"))
}

func firstCandidate(resp *genai.GenerateContentResponse) *genai.Candidate {
    if resp == nil || len(resp.Candidates) == 0 {
        return nil
    }
    return resp.Candidates[0]
}

func filterModelContent(content *genai.Content) *genai.Content {
    if content == nil {
        return nil
    }
    cleaned := &genai.Content{Role: genai.RoleModel}
    for _, part := range content.Parts {
        if part == nil || part.Thought {
            continue
        }
        cleaned.Parts = append(cleaned.Parts, part)
    }
    return cleaned
}

func collectSources(candidate *genai.Candidate) []sourceRef {
    var sources []sourceRef
    if candidate == nil {
        return sources
    }
    seen := make(map[string]bool)

    if candidate.CitationMetadata != nil {
        for _, citation := range candidate.CitationMetadata.Citations {
            if citation == nil {
                continue
            }
            uri := strings.TrimSpace(citation.URI)
            if uri == "" || seen[uri] {
                continue
            }
            seen[uri] = true
            sources = append(sources, sourceRef{Title: strings.TrimSpace(citation.Title), URI: uri})
        }
    }

    if candidate.GroundingMetadata != nil {
        for _, chunk := range candidate.GroundingMetadata.GroundingChunks {
            if chunk == nil || chunk.Web == nil {
                continue
            }
            uri := strings.TrimSpace(chunk.Web.URI)
            if uri == "" || seen[uri] {
                continue
            }
            seen[uri] = true
            sources = append(sources, sourceRef{Title: strings.TrimSpace(chunk.Web.Title), URI: uri})
        }
    }
    return sources
}

func formatCodeSnippet(index int, snippet codeSnippet) string {
    language := strings.ToLower(strings.TrimSpace(snippet.Language))
    if language == "" {
        language = "text"
    }

    var b strings.Builder
    b.WriteString(fmt.Sprintf("Code snippet %d:\n", index))
    b.WriteString("```")
    b.WriteString(language)
    b.WriteString("\n")
    b.WriteString(escapeCode(snippet.Code))
    b.WriteString("\n```")
    if snippet.Outcome != "" {
        b.WriteString("\nOutcome: ")
        b.WriteString(snippet.Outcome)
    }
    if strings.TrimSpace(snippet.Output) != "" {
        b.WriteString("\nOutput:\n```")
        b.WriteString(language)
        b.WriteString("\n")
        b.WriteString(escapeCode(snippet.Output))
        b.WriteString("\n```")
    }
    return b.String()
}

func escapeCode(text string) string {
    text = strings.ReplaceAll(text, "\\", "\\\\")
    return strings.ReplaceAll(text, "`", "\\`")
}

