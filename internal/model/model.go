// Package model holds shared types between db, web, and walk packages.
package model

// Comment is the full row shape used by the viewer and walk handlers.
type Comment struct {
	ID          string
	Repo        string
	PRNumber    int
	CommentType string
	URL         string
	Author      string
	Body        string
	DiffHunk    string
	FilePath    string
	PRTitle     string
	CreatedAt   string
	Status      string
	Decision    string
	RoutedTo    string
	Note        string
}
