package s3

import "testing"

// TestSigV4KnownVector pins the signing math against the canonical AWS
// documentation worked example (GET on examplebucket, access key
// AKIAIOSFODNN7EXAMPLE, secret wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY,
// date 20130524, region us-east-1): it reproduces exactly that request's
// canonical string and string-to-sign, then asserts the resulting
// signature matches AWS's documented value byte for byte. This is ground
// truth, not internal self-consistency.
func TestSigV4KnownVector(t *testing.T) {
	const (
		emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		amzDate          = "20130524T000000Z"
		dateStamp        = "20130524"
		region           = "us-east-1"
		secretAccessKey  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		wantSignature    = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	)

	headers := map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": emptyPayloadHash,
		"x-amz-date":           amzDate,
	}
	headerValue := func(name string) string { return headers[name] }

	cr := canonicalRequestString("GET", "/test.txt", "",
		[]string{"host", "range", "x-amz-content-sha256", "x-amz-date"},
		headerValue, emptyPayloadHash)

	wantCanonicalRequest := "GET\n" +
		"/test.txt\n" +
		"\n" +
		"host:examplebucket.s3.amazonaws.com\n" +
		"range:bytes=0-9\n" +
		"x-amz-content-sha256:" + emptyPayloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n" +
		"\n" +
		"host;range;x-amz-content-sha256;x-amz-date\n" +
		emptyPayloadHash
	if cr != wantCanonicalRequest {
		t.Fatalf("canonical request mismatch:\ngot:\n%s\nwant:\n%s", cr, wantCanonicalRequest)
	}

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	sts := stringToSign(amzDate, scope, cr)

	got := signature(secretAccessKey, dateStamp, region, "s3", sts)
	if got != wantSignature {
		t.Fatalf("signature mismatch: got %s want %s", got, wantSignature)
	}
}
