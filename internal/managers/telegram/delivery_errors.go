package telegram

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managerdelivery"
)

func isMessageNotModified(err error) bool {
	var apiErr *tgAPIError

	return errors.As(err, &apiErr) && apiErr.ErrorCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Description), "not modified")
}

func isMessageMissing(err error) bool {
	var apiErr *tgAPIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != http.StatusBadRequest {
		return false
	}

	description := strings.ToLower(apiErr.Description)

	return strings.Contains(description, "message to edit not found") ||
		strings.Contains(description, "message to delete not found")
}

func isTopicMissing(err error) bool {
	var apiErr *tgAPIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != http.StatusBadRequest {
		return false
	}

	description := strings.ToLower(apiErr.Description)

	return strings.Contains(description, "topic not found") || strings.Contains(description, "thread not found")
}

func (t *outputTransport) deliveryFailure(err error) managerdelivery.Result {
	message := logger.Redact(strings.ReplaceAll(err.Error(), "\n", " "))
	if len(message) > 512 {
		message = message[:512]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}

	var apiErr *tgAPIError
	if !errors.As(err, &apiErr) {
		return managerdelivery.Result{Retryable: true, Error: message}
	}

	if apiErr.ErrorCode == http.StatusTooManyRequests || apiErr.ErrorCode >= http.StatusInternalServerError {
		return managerdelivery.Result{
			Retryable:  true,
			Error:      message,
			RetryAfter: time.Duration(apiErr.RetryAfter) * time.Second,
		}
	}

	return managerdelivery.Result{Error: message}
}
