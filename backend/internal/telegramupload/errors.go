package telegramupload

import (
	"context"
	"errors"
	"strings"

	"github.com/nianzhibai/91/backend/internal/applog"
)

type transferError struct {
	stage   string
	message string
	cause   error
}

func (e *transferError) Error() string { return e.message }
func (e *transferError) Unwrap() error { return e.cause }

func failure(stage, message string, cause error) error {
	return &transferError{stage: stage, message: message, cause: cause}
}

func diagnosticText(err error) string {
	if detail, ok := err.(*transferError); ok && detail.cause != nil {
		return detail.message + ": " + diagnosticText(detail.cause)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var messages []string
		for _, cause := range joined.Unwrap() {
			messages = append(messages, diagnosticText(cause))
		}
		return strings.Join(messages, "; ")
	}
	return err.Error()
}

func logFailure(ctx context.Context, videoID, driveID string, err error) {
	fields := applog.Fields{Component: "telegram-upload", VideoID: videoID, DriveID: driveID}
	var detail *transferError
	if errors.As(err, &detail) {
		fields.Stage = detail.stage
	}
	cause := err
	if direct, ok := err.(*transferError); ok && direct.cause != nil {
		cause = direct.cause
	}
	applog.Error(ctx, err.Error(), failure(fields.Stage, diagnosticText(cause), cause), fields)
}
