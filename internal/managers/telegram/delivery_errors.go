package telegram

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managerdelivery"
)

func classifyDeliveryError(err error) managerdelivery.SendResult {
	message := logger.Redact(strings.ReplaceAll(err.Error(), "\n", " "))
	if len(message) > 512 {
		message = message[:512]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}

	result := managerdelivery.SendResult{Class: managerdelivery.SendTransient, Reason: message}

	var apiErr *tgAPIError
	if !errors.As(err, &apiErr) {
		return result
	}

	if apiErr.ErrorCode == http.StatusForbidden || isChatNotFound(err) {
		result.Class = managerdelivery.SendChatFailure
		return result
	}

	if apiErr.ErrorCode == http.StatusTooManyRequests || isRetryableHTTPStatus(apiErr.ErrorCode) {
		result.RetryAfter = startupRetryDelay(err, 0)
		return result
	}

	result.Class = managerdelivery.SendRowPermanent

	return result
}
