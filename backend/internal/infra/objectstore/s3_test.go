package objectstore

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3PresignGetUsesExternalEndpointAndPrefix(t *testing.T) {
	client := awss3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-access", "test-secret", ""),
	}, func(options *awss3.Options) {
		options.BaseEndpoint = aws.String("https://storage.example.test")
		options.UsePathStyle = true
	})
	store := &S3Store{
		client:    client,
		presigner: awss3.NewPresignClient(client),
		bucket:    "deeix-files",
		prefix:    "deeix",
	}

	rawURL, err := store.PresignGet(context.Background(), "user_1/recording.mp3", 2*time.Hour)
	if err != nil {
		t.Fatalf("presign get failed: %v", err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse presigned URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "storage.example.test" {
		t.Fatalf("unexpected presigned origin: %q", parsed.Scheme+"://"+parsed.Host)
	}
	if parsed.Path != "/deeix-files/deeix/user_1/recording.mp3" {
		t.Fatalf("unexpected presigned path: %q", parsed.Path)
	}
	if !strings.Contains(parsed.RawQuery, "X-Amz-Signature=") {
		t.Fatal("presigned URL is missing a signature")
	}
}

func TestS3PresignGetRejectsInvalidExpiry(t *testing.T) {
	store := &S3Store{}
	if _, err := store.PresignGet(context.Background(), "recording.mp3", 0); !errors.Is(err, ErrInvalidExpiry) {
		t.Fatalf("PresignGet() error = %v, want ErrInvalidExpiry", err)
	}
}

func TestS3ObjectKeyAppliesPrefix(t *testing.T) {
	store := &S3Store{prefix: normalizeKey("/deeix-chat/prod/")}

	if got := store.objectKey("/user_1/2026/05/file.txt"); got != "deeix-chat/prod/user_1/2026/05/file.txt" {
		t.Fatalf("unexpected object key: %q", got)
	}
}

func TestS3ObjectKeyWithoutPrefix(t *testing.T) {
	store := &S3Store{}

	if got := store.objectKey("/user_1/2026/05/file.txt"); got != "user_1/2026/05/file.txt" {
		t.Fatalf("unexpected object key: %q", got)
	}
}
