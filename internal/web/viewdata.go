package web

// HeaderData is rendered by the shared header partial on every page.
type HeaderData struct {
	LoggedIn    bool
	DisplayName string
	Username    string
	Balance     int64
}

// Page wraps shared Header + page-specific Content.
type Page[T any] struct {
	Header  HeaderData
	Content T
}
