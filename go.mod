module eteonbot

go 1.24.0

require (
	github.com/joho/godotenv v1.5.1
	google.golang.org/genai v1.25.0
	gopkg.in/telebot.v4 v4.0.0-beta.5
)

require cloud.google.com/go v0.122.0 // indirect
replace cloud.google.com/go/compute/metadata => cloud.google.com/go/compute v1.6.1
