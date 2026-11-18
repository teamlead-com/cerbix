package notify

import (
	"errors"

	"github.com/teamlead-com/cerbix/internal/deliveryfail"
)

// ErrInvalidURL is a delivery URL the request could not be built from. It names no part of the URL:
// url.Parse's own error quotes the input, secret path and query included (sec-hardening-3 T-4).
var ErrInvalidURL = errors.New("invalid URL")

// DeliveryFailure turns a failed request — one that got no HTTP response — into text that holds no
// part of the delivery URL (sec-hardening-3 T-4, S21). The client's *url.Error quotes the whole URL,
// and that text reached the outbox's last_error, the log and the dead-letter API: a Telegram bot
// token, a Slack webhook path, a tokened webhook URL, all of them write-only in the API.
//
// The reason is deliveryfail.Class, a closed set through which no library text passes, so the
// property holds by construction rather than by scrubbing. Both deliverers (notify and webhook) call
// this one copy; the `channel <id>` / `webhook <id>` prefix they add is what tells an operator which
// one failed. Phone providers keep the same class (FR-041a D34).
func DeliveryFailure(err error) error {
	return errors.New("post failed: " + deliveryfail.Class(err))
}
