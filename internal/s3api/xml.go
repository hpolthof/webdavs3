package s3api

import "encoding/xml"

// ListAllMyBucketsResult is the XML response for ListBuckets.
type ListAllMyBucketsResult struct {
	XMLName xml.Name      `xml:"ListAllMyBucketsResult"`
	Owner   Owner         `xml:"Owner"`
	Buckets []BucketEntry `xml:"Buckets>Bucket"`
}

// Owner represents the owner element in bucket listing responses.
type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// BucketEntry is a single bucket in a ListBuckets response.
type BucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

// ListBucketResult is the XML response for ListObjectsV2.
type ListBucketResult struct {
	XMLName               xml.Name      `xml:"ListBucketResult"`
	Name                  string        `xml:"Name"`
	Prefix                string        `xml:"Prefix"`
	Delimiter             string        `xml:"Delimiter,omitempty"`
	MaxKeys               int           `xml:"MaxKeys"`
	KeyCount              int           `xml:"KeyCount"`
	IsTruncated           bool          `xml:"IsTruncated"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	ContinuationToken     string        `xml:"ContinuationToken,omitempty"`
	Contents              []ObjectEntry `xml:"Contents"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes"`
}

// ObjectEntry is a single object in a ListObjectsV2 response.
type ObjectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

// CommonPrefix wraps a common prefix string.
type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// InitiateMultipartUploadResult is the XML response for CreateMultipartUpload.
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// CompleteMultipartUploadResult is the XML response for CompleteMultipartUpload.
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// ListMultipartUploadsResult is the XML response for ListMultipartUploads.
type ListMultipartUploadsResult struct {
	XMLName xml.Name      `xml:"ListMultipartUploadsResult"`
	Bucket  string        `xml:"Bucket"`
	Uploads []UploadEntry `xml:"Upload"`
}

// UploadEntry is a single in-progress upload.
type UploadEntry struct {
	UploadID  string `xml:"UploadId"`
	Key       string `xml:"Key"`
	Initiated string `xml:"Initiated"`
}

// ListPartsResult is the XML response for ListParts.
type ListPartsResult struct {
	XMLName  xml.Name    `xml:"ListPartsResult"`
	Bucket   string      `xml:"Bucket"`
	Key      string      `xml:"Key"`
	UploadID string      `xml:"UploadId"`
	Parts    []PartEntry `xml:"Part"`
}

// PartEntry is a single part in a ListParts response.
type PartEntry struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
	Size       int64  `xml:"Size"`
}

// ErrorResponse is the S3 error XML body.
type ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	RequestID string   `xml:"RequestId"`
}
