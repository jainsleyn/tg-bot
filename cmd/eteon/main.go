package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/joho/godotenv"

    "eteonbot/internal/app"
)

func main() {
    if err := godotenv.Load(); err != nil {
        if !os.IsNotExist(err) {
            log.Printf("warning: could not load .env: %v", err)
        }
    }

    cfg := app.Config{
        TelegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
        GeminiAPIKey:  os.Getenv("GEMINI_API_KEY"),
    }

    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    botApp, err := app.New(ctx, cfg)
    if err != nil {
        log.Fatalf("failed to initialise application: %v", err)
    }

    log.Println("Eteon bot is running")
    if err := botApp.Run(ctx); err != nil {
        log.Fatalf("bot stopped with error: %v", err)
    }
}
