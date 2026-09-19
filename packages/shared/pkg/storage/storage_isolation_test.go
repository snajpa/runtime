package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

// TestObjectStoreBackendsAreIsolated pins the isolation contract every
// container-backed suite depends on: each backend gets its own container and
// its own bucket, so two concurrent runs (or two tests in one binary) cannot
// see, own, or clobber each other's objects. That is the cross-run class that
// produced `409 BucketAlreadyOwnedByYou` when the harness shared one fixed
// bucket name (S-59; the per-test containers and unique names are S-54's).
//
// The check is deliberately behavioural rather than a name-shape assertion:
// the same bucket name is created on both endpoints and an object written
// through one must be invisible through the other. A harness that shares an
// emulator instance (fixed container name, reused container, shared endpoint)
// fails this even if the bucket *names* look unique.
func TestObjectStoreBackendsAreIsolated(t *testing.T) {
	t.Parallel()

	first := startObjectStoreBackend(t)
	second := startObjectStoreBackend(t)

	require.NotEqual(t, first.endpoint, second.endpoint, "each backend must run its own container")
	require.NotEqual(t, first.bucket, second.bucket, "each backend must own its own bucket")

	firstClient := first.newClient(t, nil)
	secondClient := second.newClient(t, nil)

	// The same bucket name exists on both endpoints: they are separate object
	// stores, not two handles on one.
	const sharedName = "isolation-probe"

	require.NoError(t, createBucketTolerant(t.Context(), firstClient, sharedName),
		"create the probe bucket on the first backend")
	require.NoError(t, createBucketTolerant(t.Context(), secondClient, sharedName),
		"create the probe bucket on the second backend")

	key := "isolation-probe-object"

	_, err := firstClient.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(sharedName),
		Key:    aws.String(key),
		Body:   strings.NewReader("isolation"),
	})
	require.NoError(t, err, "put the probe object on the first backend")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		_, _ = firstClient.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(sharedName),
			Key:    aws.String(key),
		})
	})

	_, err = secondClient.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(sharedName),
		Key:    aws.String(key),
	})
	require.Error(t, err, "the first backend's object must not be visible on the second backend")
}
