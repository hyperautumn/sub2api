package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func markUpstreamBusinessError(c *gin.Context, failure *service.UpstreamFailoverError, business service.UpstreamBusinessError) {
	message := service.SanitizeUpstreamErrorMessage(service.ExtractUpstreamErrorMessage(failure.ResponseBody))
	service.SetOpsUpstreamError(c, failure.StatusCode, message, "")
	service.MarkUpstreamBusinessError(c, business, failure.ResponseHeaders)
}
