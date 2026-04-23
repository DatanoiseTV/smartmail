package main

// defineTools returns the OpenAI tool-calling schema the LLM sees. Three
// mutually exclusive tools — the model picks exactly one per email. This is
// stricter than a single "classify" tool and it makes the model's intent
// unambiguous to the caller.
func defineTools(pinned []string, existingFolders []string) []tool {
	return []tool{
		{
			Type: "function",
			Function: functionSpec{
				Name: "file_email",
				Description: "Move the email into a category folder. Use this for newsletters, " +
					"marketing, order confirmations, receipts, shipping notifications, travel " +
					"bookings, social-network notifications, developer updates, bills, banking " +
					"statements, calendar invites, and any other routine, non-urgent mail that " +
					"the user does not need to see immediately but wants archived for later " +
					"reference.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"category": map[string]any{
							"type":        "string",
							"description": "Top-level category this email belongs to. Prefer an existing folder from the list provided in the system prompt. Only invent a new one when none of the existing categories is a sensible fit.",
							"enum":        pinned,
						},
						"subfolder": map[string]any{
							"type":        "string",
							"description": "Optional second-level folder under the category (e.g. 'Amazon' under 'Shopping'). Leave empty for top-level. Use ONLY when doing so creates meaningful grouping — do not create one subfolder per sender.",
						},
						"tags": map[string]any{
							"type":        "array",
							"description": "Short keyword tags (max 5, lowercase, hyphenated) to apply as IMAP keywords so the user can search semantically across folders. Example: ['invoice','aws','2026-q2']",
							"items":       map[string]any{"type": "string"},
							"maxItems":    5,
						},
						"priority": map[string]any{
							"type":        "string",
							"description": "How attention-worthy this message is even though we are archiving it.",
							"enum":        []string{"low", "normal", "high"},
						},
						"should_star": map[string]any{
							"type":        "boolean",
							"description": "Set the \\Flagged (starred) flag. Use sparingly — only for messages the user will want to revisit, such as receipts above a noticeable amount, important confirmations, or actionable items with deadlines.",
						},
						"confidence": map[string]any{
							"type":        "number",
							"description": "Your confidence in this classification, 0.0–1.0. If below the user's threshold the message will stay in the inbox instead of being moved.",
							"minimum":     0,
							"maximum":     1,
						},
						"reasoning": map[string]any{
							"type":        "string",
							"description": "One short sentence (≤160 chars) stating the strongest signal that led to this decision. Example: 'List-Id shipping@amazon.com and subject \"Your order shipped\".'",
						},
					},
					"required": []string{"category", "confidence", "reasoning"},
				},
			},
		},
		{
			Type: "function",
			Function: functionSpec{
				Name: "mark_spam",
				Description: "Move the email into the user's spam/junk folder. Use ONLY when the message is unambiguously spam, phishing, a scam, or unsolicited bulk mail from an untrusted sender. Do NOT use this for legitimate marketing the user subscribed to — that goes to 'Marketing' via file_email.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"indicators": map[string]any{
							"type":        "array",
							"description": "Concrete spam signals you identified (e.g. 'sender domain mismatch', 'urgency + credential request', 'unverifiable prize claim').",
							"items":       map[string]any{"type": "string"},
						},
						"confidence": map[string]any{
							"type":    "number",
							"minimum": 0,
							"maximum": 1,
						},
						"reasoning": map[string]any{
							"type":        "string",
							"description": "One sentence explaining why this is spam.",
						},
					},
					"required": []string{"reasoning", "confidence"},
				},
			},
		},
		{
			Type: "function",
			Function: functionSpec{
				Name: "keep_in_inbox",
				Description: "Leave the email in the inbox untouched. Use for personal correspondence, anything requiring a reply, time-sensitive requests, messages from known human contacts, account-security alerts (password resets, suspicious-login notifications), and anything where getting it wrong would cost the user real attention. When in doubt, prefer this over file_email.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"priority": map[string]any{
							"type":        "string",
							"description": "How urgent this message is.",
							"enum":        []string{"normal", "high", "urgent"},
						},
						"should_flag": map[string]any{
							"type":        "boolean",
							"description": "Set the \\Flagged (starred) flag to surface it visually.",
						},
						"tags": map[string]any{
							"type":     "array",
							"items":    map[string]any{"type": "string"},
							"maxItems": 5,
						},
						"reasoning": map[string]any{
							"type":        "string",
							"description": "One short sentence on why this needs the user's attention.",
						},
					},
					"required": []string{"reasoning"},
				},
			},
		},
	}
}

// Decision is the parsed tool call the classifier turns into IMAP actions.
type Decision struct {
	Tool       string   // file_email | mark_spam | keep_in_inbox
	Category   string   // for file_email
	Subfolder  string   // for file_email
	Tags       []string // for file_email or keep_in_inbox
	Priority   string
	Star       bool
	Confidence float64
	Reasoning  string
	Indicators []string // for mark_spam
}
