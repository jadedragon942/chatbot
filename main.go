package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	irc "github.com/thoj/go-ircevent"
)

// Config holds the bot configuration
type Config struct {
	Server         string
	Port           string
	Channel        string
	Nick           string
	BotName        string
	Persona        string
	TriggerPattern *regexp.Regexp
}

// Message represents a chat message for context
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// IRCBot represents our IRC bot
type IRCBot struct {
	config     *Config
	connection *irc.Connection
	client     *http.Client
	context    []Message // Keep conversation context
}

// NewIRCBot creates a new IRC bot instance
func NewIRCBot(config *Config) *IRCBot {
	conn := irc.IRC(config.Nick, config.BotName)
	conn.VerboseCallbackHandler = false
	conn.Debug = false

	conn.UseTLS = true
	conn.TLSConfig = &tls.Config{
		InsecureSkipVerify: true,
	}

	bot := &IRCBot{
		config:     config,
		connection: conn,
		client:     &http.Client{Timeout: 30 * time.Second},
		context:    make([]Message, 0, 20), // Keep last 20 messages for context
	}

	// Add system message with persona
	if config.Persona != "" {
		bot.context = append(bot.context, Message{
			Role:    "system",
			Content: config.Persona,
		})
	}

	return bot
}

// Connect connects to the IRC server and joins the channel
func (bot *IRCBot) Connect() error {
	// Set up event handlers
	bot.setupEventHandlers()

	// Connect to server
	err := bot.connection.Connect(fmt.Sprintf("%s:%s", bot.config.Server, bot.config.Port))
	if err != nil {
		return fmt.Errorf("failed to connect to IRC server: %v", err)
	}

	return nil
}

// setupEventHandlers configures IRC event handlers
func (bot *IRCBot) setupEventHandlers() {
	// Handle successful connection
	bot.connection.AddCallback("001", func(e *irc.Event) {
		log.Printf("Connected to %s", bot.config.Server)
		bot.connection.Join(bot.config.Channel)
	})

	// Handle joining channel
	bot.connection.AddCallback("JOIN", func(e *irc.Event) {
		if e.Nick == bot.config.Nick {
			log.Printf("Joined channel %s", bot.config.Channel)
		}
	})

	// Handle private messages and channel messages
	bot.connection.AddCallback("PRIVMSG", func(e *irc.Event) {
		bot.handleMessage(e)
	})

	// Handle disconnection
	bot.connection.AddCallback("DISCONNECTED", func(e *irc.Event) {
		log.Println("Disconnected from server")
	})

	// Handle errors
	bot.connection.AddCallback("ERROR", func(e *irc.Event) {
		log.Printf("IRC Error: %s", e.Message())
	})
}

// handleMessage processes incoming IRC messages
func (bot *IRCBot) handleMessage(e *irc.Event) {
	nick := e.Nick
	target := e.Arguments[0]
	message := e.Message()

	// Skip messages from the bot itself
	if nick == bot.config.Nick {
		return
	}

	// Determine if this is a private message or channel message
	isPrivateMessage := target == bot.config.Nick
	shouldRespond := isPrivateMessage || bot.shouldRespondToMessage(message)

	if !shouldRespond {
		return
	}

	log.Printf("Processing message from %s: %s", nick, message)

	// Clean the message (remove bot mentions)
	cleanMessage := bot.cleanMessage(message)

	// Get AI response
	response, err := bot.getAIResponse(cleanMessage, nick)
	if err != nil {
		log.Printf("Error getting AI response: %v", err)
		return
	}

	// Send response back
	responseTarget := target
	if isPrivateMessage {
		responseTarget = nick
	}

	// Split long responses into multiple lines
	bot.sendResponse(responseTarget, response)
}

// shouldRespondToMessage determines if the bot should respond to a channel message
func (bot *IRCBot) shouldRespondToMessage(message string) bool {
	// Always respond if mentioned by name
	if strings.Contains(strings.ToLower(message), strings.ToLower(bot.config.Nick)) {
		return true
	}

	// Use trigger pattern if configured
	if bot.config.TriggerPattern != nil {
		return bot.config.TriggerPattern.MatchString(message)
	}

	return false
}

// cleanMessage removes bot mentions and cleans up the message
func (bot *IRCBot) cleanMessage(message string) string {
	// Remove direct mentions of the bot
	cleanMsg := regexp.MustCompile(`(?i)`+regexp.QuoteMeta(bot.config.Nick)+`[,:]\s*`).ReplaceAllString(message, "")
	cleanMsg = strings.TrimSpace(cleanMsg)

	if cleanMsg == "" {
		return "Hello!"
	}

	return cleanMsg
}

// getAIResponse gets a response from Pollinations.ai
func (bot *IRCBot) getAIResponse(message, fromNick string) (string, error) {
	// Add user message to context
	userMessage := Message{
		Role:    "user",
		Content: fmt.Sprintf("%s: %s", fromNick, message),
	}

	bot.addToContext(userMessage)

	// Build the prompt from context
	prompt := bot.buildPrompt()

	// Make API request to Pollinations.ai
	// Using URL encoding for the prompt
	encodedPrompt := url.QueryEscape(prompt)
	apiUrl := fmt.Sprintf("https://text.pollinations.ai/%s", encodedPrompt)

	resp, err := bot.client.Get(apiUrl)
	if err != nil {
		return "", fmt.Errorf("failed to make API request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	fmt.Printf("body -> %s\n", string(body))

	// Pollinations.ai returns plain text, not JSON
	aiResponse := strings.TrimSpace(string(body))

	// Clean up common artifacts
	aiResponse = bot.cleanAIResponse(aiResponse)

	// Add AI response to context
	bot.addToContext(Message{
		Role:    "assistant",
		Content: aiResponse,
	})

	return aiResponse, nil
}

// buildPrompt creates a prompt from the conversation context
func (bot *IRCBot) buildPrompt() string {
	var promptBuilder strings.Builder

	for _, msg := range bot.context {
		switch msg.Role {
		case "system":
			promptBuilder.WriteString("System: ")
			promptBuilder.WriteString(msg.Content)
			promptBuilder.WriteString("\n")
		case "user":
			promptBuilder.WriteString("User: ")
			promptBuilder.WriteString(msg.Content)
			promptBuilder.WriteString("\n")
		case "assistant":
			promptBuilder.WriteString("Assistant: ")
			promptBuilder.WriteString(msg.Content)
			promptBuilder.WriteString("\n")
		}
	}

	promptBuilder.WriteString("Assistant: ")
	return promptBuilder.String()
}

// cleanAIResponse cleans up the AI response
func (bot *IRCBot) cleanAIResponse(response string) string {
	// Remove common prefixes that might appear
	prefixes := []string{"Assistant: ", "Bot: ", "AI: "}
	for _, prefix := range prefixes {
		if strings.HasPrefix(response, prefix) {
			response = strings.TrimPrefix(response, prefix)
			break
		}
	}

	// Clean up any HTML tags if present
	response = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(response, "")

	// Remove excessive whitespace
	response = regexp.MustCompile(`\s+`).ReplaceAllString(response, " ")

	return strings.TrimSpace(response)
}

// addToContext adds a message to the conversation context with size limiting
func (bot *IRCBot) addToContext(message Message) {
	bot.context = append(bot.context, message)

	// Keep context size reasonable (keep system message + last 18 messages)
	if len(bot.context) > 19 {
		// Keep system message at index 0, remove oldest user/assistant messages
		systemMsg := bot.context[0]
		bot.context = append([]Message{systemMsg}, bot.context[len(bot.context)-18:]...)
	}
}

// sendResponse sends a response to IRC, handling long messages
func (bot *IRCBot) sendResponse(target, response string) {
	// Split long messages
	maxLength := 400 // Leave some room for IRC protocol overhead

	if len(response) <= maxLength {
		bot.connection.Privmsg(target, response)
		return
	}

	// Split by sentences first, then by words if needed
	sentences := regexp.MustCompile(`[.!?]+\s+`).Split(response, -1)
	currentMsg := ""

	for _, sentence := range sentences {
		if len(currentMsg)+len(sentence)+1 <= maxLength {
			if currentMsg != "" {
				currentMsg += " "
			}
			currentMsg += sentence
		} else {
			if currentMsg != "" {
				bot.connection.Privmsg(target, currentMsg)
				time.Sleep(500 * time.Millisecond) // Small delay between messages
			}

			// If single sentence is too long, split by words
			if len(sentence) > maxLength {
				words := strings.Fields(sentence)
				currentMsg = ""
				for _, word := range words {
					if len(currentMsg)+len(word)+1 <= maxLength {
						if currentMsg != "" {
							currentMsg += " "
						}
						currentMsg += word
					} else {
						if currentMsg != "" {
							bot.connection.Privmsg(target, currentMsg)
							time.Sleep(500 * time.Millisecond)
						}
						currentMsg = word
					}
				}
			} else {
				currentMsg = sentence
			}
		}
	}

	if currentMsg != "" {
		bot.connection.Privmsg(target, currentMsg)
	}
}

// Start starts the bot's main loop
func (bot *IRCBot) Start() {
	bot.connection.Loop()
}

// Stop disconnects the bot
func (bot *IRCBot) Stop() {
	bot.connection.Quit()
	bot.connection.Disconnect()
}

func main() {
	// Configuration - you can modify these or use environment variables
	config := &Config{
		Server:  getEnvOrDefault("IRC_SERVER", "irc.h4ks.com"),
		Port:    getEnvOrDefault("IRC_PORT", "6697"),
		Channel: getEnvOrDefault("IRC_CHANNEL", "#lobby"),
		Nick:    getEnvOrDefault("IRC_NICK", "SteveBot"),
		BotName: getEnvOrDefault("IRC_BOTNAME", "Very cool and helpful bot"),
		Persona: getEnvOrDefault("BOT_PERSONA", "You are a helpful and friendly IRC bot named Steve. Keep responses concise and engaging. You have a casual, slightly witty personality. Always be respectful and helpful."),
	}

	// Optional: Set up trigger pattern for channel messages
	// This example responds to messages containing "bot" or starting with "!"
	triggerPattern := getEnvOrDefault("TRIGGER_PATTERN", `(?i)(steve|^!)`)
	if triggerPattern != "" {
		pattern, err := regexp.Compile(triggerPattern)
		if err != nil {
			log.Printf("Invalid trigger pattern: %v", err)
		} else {
			config.TriggerPattern = pattern
		}
	}

	// Create and start bot
	bot := NewIRCBot(config)

	log.Printf("Starting IRC bot...")
	log.Printf("Server: %s:%s", config.Server, config.Port)
	log.Printf("Channel: %s", config.Channel)
	log.Printf("Nick: %s", config.Nick)

	err := bot.Connect()
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}

	// Start the bot (this will block)
	bot.Start()
}

// getEnvOrDefault gets an environment variable or returns a default value
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
