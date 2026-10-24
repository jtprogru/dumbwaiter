package s3

import (
	"testing"

	"github.com/jtprogru/dumbwaiter/internal/config"
)

func TestObjectURL(t *testing.T) {
	tests := []struct {
		endpoint  string
		pathStyle bool
		key       string
		want      string
	}{
		{"https://storage.yandexcloud.net", true, "index.html", "https://storage.yandexcloud.net/site/index.html"},
		{"https://s3.eu-west-1.amazonaws.com", false, "docs/index.html", "https://site.s3.eu-west-1.amazonaws.com/docs/index.html"},
		{"http://minio.storage.svc:9000", true, "a b/ü+(1).html", "http://minio.storage.svc:9000/site/a%20b/%C3%BC%2B%281%29.html"},
		{"http://gateway/s3/", true, "x", "http://gateway/s3/site/x"},
	}
	for _, tt := range tests {
		c, err := New(config.S3{Endpoint: tt.endpoint, Region: "r", PathStyle: tt.pathStyle})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.objectURL("site", tt.key); got != tt.want {
			t.Errorf("objectURL(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}
