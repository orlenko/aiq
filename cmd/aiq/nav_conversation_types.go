package main

// Explicit transport values; never embed a private projection or parsed Session.
// Groups keep the paint recipe and headers at their original cut boundaries.
type navWireTime struct {
	MS    int64 `json:"ms"`
	Known bool  `json:"known"`
}

type navConversationWindow struct {
	Session       navConversationSession   `json:"session"`
	EarlierHidden int                      `json:"earlier_hidden"`
	LaterHidden   int                      `json:"later_hidden"`
	WindowStart   int                      `json:"window_start"`
	Reset         bool                     `json:"reset"`
	Markup        bool                     `json:"markup"`
	Messages      []navConversationMessage `json:"messages"`
}

type navConversationSession struct {
	Provider string      `json:"provider"`
	ID       string      `json:"id"`
	Cwd      string      `json:"cwd"`
	Title    string      `json:"title"`
	Model    string      `json:"model"`
	Branch   string      `json:"branch"`
	Started  navWireTime `json:"started"`
	Updated  navWireTime `json:"updated"`
	Worker   bool        `json:"worker"`
	Bypass   bool        `json:"bypass"`
}

type navConversationMessage struct {
	ID           string                 `json:"id"`
	Key          string                 `json:"key"`
	Kind         string                 `json:"kind"`
	Sub          string                 `json:"sub"`
	Turn         int                    `json:"turn"`
	At           navWireTime            `json:"at"`
	SourceKnown  bool                   `json:"source_known"`
	Source       string                 `json:"source"`
	AccountKnown bool                   `json:"account_known"`
	Account      string                 `json:"account"`
	Raw          string                 `json:"raw"`
	Groups       []navConversationGroup `json:"groups"`
}

type navConversationGroup struct {
	Ordinal       int                   `json:"ordinal"`
	Visible       bool                  `json:"visible"`
	LeadingBreak  bool                  `json:"leading_break"`
	EmptyBodyLine bool                  `json:"empty_body_line"`
	Head          navConversationHead   `json:"head"`
	Base          []string              `json:"base"`
	Lines         []navConversationLine `json:"lines"`
}

type navConversationHead struct {
	Mode          string      `json:"mode"`
	Who           string      `json:"who"`
	ClockMode     string      `json:"clock_mode"`
	ClockText     string      `json:"clock_text"`
	At            navWireTime `json:"at"`
	DurationKnown bool        `json:"duration_known"`
	DurationMS    int64       `json:"duration_ms"`
	Tools         int         `json:"tools"`
}

type navConversationLine struct {
	Kind      string                `json:"kind"`
	TableRows int                   `json:"table_rows"`
	Spans     []navConversationSpan `json:"spans"`
}

type navConversationSpan struct {
	Classes    []string `json:"c"`
	Decorative bool     `json:"d"`
	Text       string   `json:"t"`
}
