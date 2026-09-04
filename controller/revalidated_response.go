package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// etagVersionPublicContent namespaces the public-content ETag; bump it when
// the JSON envelope served by serveRevalidatedJSON changes shape.
const etagVersionPublicContent = "public-content:v2"

// serveRevalidatedJSON writes public content as JSON with a weak
// content-derived ETag and answers conditional requests with 304 Not
// Modified. The ETag is a weak validator derived from the full envelope, so it
// is stable across replicas and JSON encodings, and a new one is issued when
// the content or any extra envelope field changes. Cache-Control: no-cache
// forces revalidation before reuse, so an admin edit takes effect on the next
// request; Vary: Accept-Encoding keeps the gzip and identity encodings apart in
// shared caches. extraFields are merged into the top-level envelope and
// participate in the ETag so fork-only fields (e.g. home_page_style) trigger a
// correct revalidation when they change.
func serveRevalidatedJSON(c *gin.Context, content string, extraFields ...map[string]any) {
	body := map[string]any{
		"success": true,
		"message": "",
		"data":    content,
	}
	for _, m := range extraFields {
		for k, v := range m {
			body[k] = v
		}
	}
	respBody, err := common.Marshal(body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	etag := common.ETagFor(etagVersionPublicContent, string(respBody))

	c.Header("ETag", etag)
	c.Header("Cache-Control", "no-cache")
	c.Header("Vary", "Accept-Encoding")

	if common.ETagMatches(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		return
	}

	c.Data(http.StatusOK, "application/json; charset=utf-8", respBody)
}
