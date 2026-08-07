package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/pilat/coagent/internal/ctl"
)

// driverPrompt is the fixed set of provider drivers. It is a list, not free
// text: a typo here becomes a config the daemon refuses to start on.
var driverPrompt = []string{"anthropic", "openrouter", "openai", "google-sa"}

// collectProvider asks for exactly the fields the chosen driver needs.
func collectProvider() (ctl.SetProviderParams, bool) {
	driver, ok := chooseDriver()
	if !ok {
		return ctl.SetProviderParams{}, false
	}

	p := ctl.SetProviderParams{Driver: driver}

	p.Name = ask("Name for this provider", driver)
	if p.Name == "" {
		return p, false
	}

	switch driver {
	case "anthropic":
		p.APIKey = askSecret("API key")
	case "openrouter":
		p.APIKey = askSecret("API key")
		p.BaseURL = ask("Base URL", "https://openrouter.ai/api/v1")
	case "openai":
		p.BaseURL = ask("Base URL", "https://api.openai.com/v1")
		p.APIKey = askSecret("API key")
		// A bare openai endpoint has no vendor to key on, so there is no model to
		// recommend — the id has to be asked for.
		if id := ask("Model id to enable", ""); id != "" {
			p.Models = []string{id}
		}
	case "google-sa":
		p.SAFile = ask("Service-account JSON path", "")
		p.BaseURL = ask("Base URL", "")
	}

	return p, p.Name != ""
}

func chooseDriver() (string, bool) {
	fmt.Println("Which provider?")

	for i, d := range driverPrompt {
		fmt.Printf("  %d) %s\n", i+1, d)
	}

	for range 3 {
		answer := ask("Choose 1-4", "1")

		for i, d := range driverPrompt {
			if answer == d || answer == strconv.Itoa(i+1) {
				return d, true
			}
		}

		fmt.Println("Pick one of the listed drivers.")
	}

	return "", false
}

func ask(question, fallback string) string {
	if fallback != "" {
		fmt.Printf("%s [%s]: ", question, fallback)
	} else {
		fmt.Printf("%s: ", question)
	}

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return fallback
	}

	answer := strings.TrimSpace(line)
	if answer == "" {
		return fallback
	}

	return answer
}

// askSecret reads without echo. The value crosses the socket once and lands in
// ~/.coagent/secrets; config.yaml only ever holds the ${VAR} reference.
func askSecret(question string) string {
	fmt.Printf("%s (hidden): ", question)

	value, err := term.ReadPassword(int(os.Stdin.Fd()))

	fmt.Println()

	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(value))
}

func confirm(question string) bool {
	answer := strings.ToLower(ask(question+" [Y/n]", "y"))

	return answer == "y" || answer == "yes"
}
