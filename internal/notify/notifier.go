package notify

import "context"

// Notifier sends notifications to admins or public channels.
type Notifier interface {
	NotifyAdmins(ctx context.Context, msg string)
	NotifyGroup(ctx context.Context, msg string)
	NotifyUser(ctx context.Context, userID string, msg string)
}

// Noop is a no-op notifier.
type Noop struct{}

func (Noop) NotifyAdmins(context.Context, string)       {}
func (Noop) NotifyGroup(context.Context, string)        {}
func (Noop) NotifyUser(context.Context, string, string) {}
