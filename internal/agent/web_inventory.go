package agent

import (
	"fmt"
	"strings"

	"github.com/carlvincetan/argus/internal/audit"
	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/screen"
)

// Web UIs give the auditor something a terminal cannot: the interface's own
// structure. A browser can say "this page has a password field and a submit
// button", "these rows have no link or action", "this control is disabled",
// which turns feature detection from phrase matching into reading the UI.
//
// This file maps a DOM snapshot onto the same feature vocabulary the terminal
// path uses, so the expectation table, gap analysis and reporting stay identical
// across TUI, X11 GUI and web subjects.

// observeDOM records everything the page structure proves about the application.
func observeDOM(inventory *audit.Inventory, snapshot harness.DOMSnapshot, screenName string, gridID int) {
	if inventory == nil {
		return
	}
	// Text first: prose mentions are recorded as hints, not as capabilities.
	inventory.ObserveText(snapshot.Text, screenName, gridID)

	names := make([]string, 0, len(snapshot.Elements))
	for _, element := range snapshot.Elements {
		names = append(names, strings.ToLower(strings.TrimSpace(element.Name)))
	}
	hasName := func(words ...string) bool {
		for _, name := range names {
			for _, word := range words {
				if name == "" {
					continue
				}
				if strings.Contains(name, word) {
					return true
				}
			}
		}
		return false
	}

	// A credential form is structural: a password field plus something that
	// submits it.
	if snapshot.PasswordFields > 0 {
		submit := hasName("sign in", "log in", "login", "submit", "continue", "enter", "next")
		if !submit {
			for _, element := range snapshot.Elements {
				if element.Type == "submit" {
					submit = true
					break
				}
			}
		}
		if submit {
			inventory.Record(audit.Evidence{
				Feature:  audit.Auth,
				Screen:   screenName,
				Grid:     gridID,
				Detail:   "the page contains a password field and a way to submit it",
				Strength: audit.StrengthStructural,
			})
		}
	}
	if snapshot.Forms > 0 {
		inventory.Record(audit.Evidence{
			Feature:  audit.Input,
			Screen:   screenName,
			Grid:     gridID,
			Detail:   fmt.Sprintf("the page has %d form(s)", snapshot.Forms),
			Strength: audit.StrengthStructural,
		})
	}
	if snapshot.Rows >= 3 {
		inventory.Record(audit.Evidence{
			Feature:  audit.List,
			Screen:   screenName,
			Grid:     gridID,
			Detail:   fmt.Sprintf("the page renders %d rows", snapshot.Rows),
			Strength: audit.StrengthStructural,
		})
	}
	if len(snapshot.Links) >= 2 {
		inventory.Record(audit.Evidence{
			Feature:  audit.Navigation,
			Screen:   screenName,
			Grid:     gridID,
			Detail:   fmt.Sprintf("the page links to %d other locations", len(snapshot.Links)),
			Strength: audit.StrengthAdvertised,
		})
	}
	if len(snapshot.ErrorTexts) > 0 {
		inventory.Record(audit.Evidence{
			Feature:  audit.ErrorState,
			Screen:   screenName,
			Grid:     gridID,
			Detail:   fmt.Sprintf("the page is showing an alert: %q", truncateForNote(snapshot.ErrorTexts[0], 60)),
			Strength: audit.StrengthStructural,
		})
	}
	if snapshot.ContainerDepth >= 3 {
		inventory.Structure.MaxNesting = snapshot.ContainerDepth
		inventory.Structure.NestingScreens = append(inventory.Structure.NestingScreens,
			fmt.Sprintf("%s (dom depth %d)", screenName, snapshot.ContainerDepth))
	}
	if snapshot.Dialogs > 0 {
		inventory.Record(audit.Evidence{
			Feature:  audit.Confirm,
			Screen:   screenName,
			Grid:     gridID,
			Detail:   "a dialog is open",
			Strength: audit.StrengthStructural,
		})
	}

	// Advertised capabilities, from element labels and URLs.
	advertised := []struct {
		feature audit.Feature
		words   []string
	}{
		{audit.PasswordReset, []string{"forgot password", "forgot your password", "reset password", "reset link", "recover"}},
		{audit.Signup, []string{"sign up", "signup", "register", "create account"}},
		{audit.TwoFactor, []string{"two-factor", "2fa", "verification code", "authenticator"}},
		{audit.SessionEnd, []string{"sign out", "log out", "logout"}},
		{audit.Detail, []string{"detail", "details", "view", "open", "inspect", "/item/", "/users/", "/connections/"}},
		{audit.Create, []string{"new", "add", "create", "compose"}},
		{audit.Edit, []string{"edit", "rename", "modify", "update"}},
		{audit.Delete, []string{"delete", "remove", "archive", "trash"}},
		{audit.Search, []string{"search", "find", "query"}},
		{audit.Filter, []string{"filter", "sort", "group by"}},
		{audit.Pagination, []string{"next page", "previous page", "load more", "page "}},
		{audit.Settings, []string{"settings", "preferences", "configure", "options"}},
		{audit.Help, []string{"help", "docs", "documentation", "support"}},
		{audit.Notifications, []string{"notifications", "inbox", "alerts", "messages"}},
		{audit.Export, []string{"export", "download", "save as"}},
		{audit.Confirm, []string{"are you sure", "confirm", "yes, delete", "keep"}},
	}
	// Element labels alone prove an affordance; URLs and the page text are
	// weaker, so only labels and hrefs are used here.
	evidence := append([]string{}, names...)
	for _, element := range snapshot.Elements {
		if element.Href != "" {
			evidence = append(evidence, strings.ToLower(element.Href))
		}
	}
	for _, entry := range advertised {
		for _, text := range evidence {
			matched := false
			for _, word := range entry.words {
				if strings.Contains(text, word) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			inventory.Record(audit.Evidence{
				Feature:  entry.feature,
				Screen:   screenName,
				Grid:     gridID,
				Detail:   fmt.Sprintf("the interface offers %q", truncateForNote(text, 50)),
				Strength: audit.StrengthAdvertised,
			})
			break
		}
	}
	for _, heading := range snapshot.Headings {
		inventory.Record(audit.Evidence{
			Feature:  audit.Navigation,
			Screen:   screenName,
			Grid:     gridID,
			Region:   screen.Region{},
			Detail:   fmt.Sprintf("heading %q", truncateForNote(heading, 40)),
			Strength: audit.StrengthProse,
		})
	}
}

// webActions derives the action space from the page's interactive elements:
// clicking a real button beats guessing coordinates.
func webActions(snapshot harness.DOMSnapshot, width, height int) []guiAction {
	actions := []guiAction{}
	seen := map[string]bool{}
	for index, element := range snapshot.Elements {
		if element.Disabled || element.W < 4 || element.H < 4 {
			continue
		}
		label := strings.TrimSpace(element.Name)
		if label == "" && element.Tag == "input" {
			label = element.Type + " field"
		}
		if label == "" {
			continue
		}
		key := fmt.Sprintf("%s|%s|%d|%d", element.Tag, strings.ToLower(label), element.X, element.Y)
		if seen[key] {
			continue
		}
		seen[key] = true
		actions = append(actions, guiAction{
			ID:    fmt.Sprintf("dom:%d", index),
			Kind:  "click",
			Point: [2]int{element.X + element.W/2, element.Y + element.H/2},
			Label: fmt.Sprintf("click the %s %q", element.Tag, truncateForNote(label, 40)),
		})
		if len(actions) >= 18 {
			break
		}
	}
	// Typing is only useful where there is a field to type into.
	for index, element := range snapshot.Elements {
		if element.Tag != "input" && element.Tag != "textarea" {
			continue
		}
		point := [2]int{element.X + element.W/2, element.Y + element.H/2}
		actions = append(actions, guiAction{
			ID:    fmt.Sprintf("type:%d", index),
			Kind:  "type",
			Point: point,
			Label: fmt.Sprintf("type a value into the %s field", element.Type),
		})
		if len(actions) >= 26 {
			break
		}
	}
	for _, key := range []string{"TAB", "ENTER", "ESC", "DOWN", "UP"} {
		actions = append(actions, guiAction{ID: "key:" + key, Kind: "key", Key: key, Label: "press " + key})
	}
	for _, size := range [][2]int{{1024, 768}, {640, 800}, {width, height}} {
		actions = append(actions, guiAction{
			ID:    fmt.Sprintf("resize:%dx%d", size[0], size[1]),
			Kind:  "resize",
			Size:  size,
			Label: fmt.Sprintf("resize the viewport to %dx%d", size[0], size[1]),
		})
	}
	actions = append(actions,
		guiAction{ID: "navigate:home", Kind: "navigate", Label: "go back to the starting page"},
		guiAction{ID: "idle", Kind: "idle", Label: "wait and watch the page for animation problems"},
	)
	return actions
}

func truncateForNote(text string, limit int) string {
	cleaned := strings.Join(strings.Fields(text), " ")
	runes := []rune(cleaned)
	if len(runes) <= limit {
		return cleaned
	}
	return string(runes[:limit]) + "..."
}
