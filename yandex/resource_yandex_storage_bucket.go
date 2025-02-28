package yandex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	awspolicy "github.com/jen20/awspolicyequivalence"
	storagepb "github.com/yandex-cloud/go-genproto/yandex/cloud/storage/v1"
	"github.com/yandex-cloud/terraform-provider-yandex/yandex/internal/hashcode"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

const (
	storageClassStandard = s3.StorageClassStandardIa
	storageClassCold     = "COLD"
	storageClassIce      = "ICE"
)

var storageClassSet = []string{
	storageClassStandard,
	storageClassCold,
	storageClassIce,
}

func resourceYandexStorageBucket() *schema.Resource {
	return &schema.Resource{
		Create: resourceYandexStorageBucketCreate,
		Read:   resourceYandexStorageBucketRead,
		Update: resourceYandexStorageBucketUpdate,
		Delete: resourceYandexStorageBucketDelete,

		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		SchemaVersion: 0,

		Schema: map[string]*schema.Schema{
			"bucket": {
				Type:          schema.TypeString,
				Optional:      true,
				Computed:      true,
				ForceNew:      true,
				ConflictsWith: []string{"bucket_prefix"},
			},
			"bucket_prefix": {
				Type:          schema.TypeString,
				Optional:      true,
				ForceNew:      true,
				ConflictsWith: []string{"bucket"},
			},
			"bucket_domain_name": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"access_key": {
				Type:     schema.TypeString,
				Optional: true,
			},

			"secret_key": {
				Type:      schema.TypeString,
				Optional:  true,
				Sensitive: true,
			},

			"acl": {
				Type:          schema.TypeString,
				Optional:      true,
				Computed:      true,
				ConflictsWith: []string{"grant"},
				ValidateFunc:  validation.StringInSlice(bucketACLAllowedValues, false),
			},

			"grant": {
				Type:          schema.TypeSet,
				Optional:      true,
				Computed:      true,
				Set:           grantHash,
				ConflictsWith: []string{"acl"},
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"id": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"type": {
							Type:     schema.TypeString,
							Required: true,
							ValidateFunc: validation.StringInSlice([]string{
								s3.TypeCanonicalUser,
								s3.TypeGroup,
							}, false),
						},
						"uri": {
							Type:     schema.TypeString,
							Optional: true,
						},

						"permissions": {
							Type:     schema.TypeSet,
							Required: true,
							Set:      schema.HashString,
							Elem: &schema.Schema{
								Type: schema.TypeString,
								ValidateFunc: validation.StringInSlice([]string{
									s3.PermissionFullControl,
									s3.PermissionRead,
									s3.PermissionWrite,
								}, false),
							},
						},
					},
				},
			},

			"policy": {
				Type:             schema.TypeString,
				Optional:         true,
				ValidateFunc:     validateStringIsJSON,
				DiffSuppressFunc: suppressEquivalentAwsPolicyDiffs,
			},

			"cors_rule": {
				Type:     schema.TypeList,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"allowed_headers": {
							Type:     schema.TypeList,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
						"allowed_methods": {
							Type:     schema.TypeList,
							Required: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
						"allowed_origins": {
							Type:     schema.TypeList,
							Required: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
						"expose_headers": {
							Type:     schema.TypeList,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
						"max_age_seconds": {
							Type:     schema.TypeInt,
							Optional: true,
						},
					},
				},
			},

			"website": {
				Type:     schema.TypeList,
				Optional: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"index_document": {
							Type:     schema.TypeString,
							Optional: true,
						},

						"error_document": {
							Type:     schema.TypeString,
							Optional: true,
						},

						"redirect_all_requests_to": {
							Type: schema.TypeString,
							ConflictsWith: []string{
								"website.0.index_document",
								"website.0.error_document",
								"website.0.routing_rules",
							},
							Optional: true,
						},

						"routing_rules": {
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validateStringIsJSON,
							StateFunc: func(v interface{}) string {
								json, _ := NormalizeJsonString(v)
								return json
							},
						},
					},
				},
			},
			"website_endpoint": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
			},
			"website_domain": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
			},

			"versioning": {
				Type:     schema.TypeList,
				Optional: true,
				Computed: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"enabled": {
							Type:     schema.TypeBool,
							Optional: true,
							Default:  false,
						},
					},
				},
			},

			"object_lock_configuration": {
				Type:     schema.TypeList,
				Optional: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"object_lock_enabled": {
							Type:         schema.TypeString,
							Optional:     true,
							Default:      s3.ObjectLockEnabledEnabled,
							ValidateFunc: validation.StringInSlice(s3.ObjectLockEnabled_Values(), false),
						},
						"rule": {
							Type:     schema.TypeList,
							Optional: true,
							MaxItems: 1,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"default_retention": {
										Type:     schema.TypeList,
										Required: true,
										MaxItems: 1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"mode": {
													Type:         schema.TypeString,
													Required:     true,
													ValidateFunc: validation.StringInSlice(s3.ObjectLockRetentionMode_Values(), false),
												},
												"days": {
													Type:     schema.TypeInt,
													Optional: true,
													ExactlyOneOf: []string{
														"object_lock_configuration.0.rule.0.default_retention.0.days",
														"object_lock_configuration.0.rule.0.default_retention.0.years",
													},
												},
												"years": {
													Type:     schema.TypeInt,
													Optional: true,
													ExactlyOneOf: []string{
														"object_lock_configuration.0.rule.0.default_retention.0.days",
														"object_lock_configuration.0.rule.0.default_retention.0.years",
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},

			"logging": {
				Type:     schema.TypeSet,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"target_bucket": {
							Type:     schema.TypeString,
							Required: true,
						},
						"target_prefix": {
							Type:     schema.TypeString,
							Optional: true,
						},
					},
				},
				Set: func(v interface{}) int {
					var buf bytes.Buffer
					m := v.(map[string]interface{})
					buf.WriteString(fmt.Sprintf("%s-", m["target_bucket"]))
					buf.WriteString(fmt.Sprintf("%s-", m["target_prefix"]))
					return hashcode.String(buf.String())
				},
			},

			"lifecycle_rule": {
				Type:     schema.TypeList,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"id": {
							Type:         schema.TypeString,
							Optional:     true,
							Computed:     true,
							ValidateFunc: validation.StringLenBetween(0, 255),
						},
						"prefix": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"tags": tagsSchema(),
						"enabled": {
							Type:     schema.TypeBool,
							Required: true,
						},
						"abort_incomplete_multipart_upload_days": {
							Type:     schema.TypeInt,
							Optional: true,
						},
						"expiration": {
							Type:     schema.TypeList,
							Optional: true,
							MaxItems: 1,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"date": {
										Type:         schema.TypeString,
										Optional:     true,
										ValidateFunc: validateS3BucketLifecycleTimestamp,
									},
									"days": {
										Type:         schema.TypeInt,
										Optional:     true,
										ValidateFunc: validation.IntAtLeast(0),
									},
									"expired_object_delete_marker": {
										Type:     schema.TypeBool,
										Optional: true,
									},
								},
							},
						},
						"noncurrent_version_expiration": {
							Type:     schema.TypeList,
							MaxItems: 1,
							Optional: true,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"days": {
										Type:         schema.TypeInt,
										Optional:     true,
										ValidateFunc: validation.IntAtLeast(1),
									},
								},
							},
						},
						"transition": {
							Type:     schema.TypeSet,
							Optional: true,
							Set:      transitionHash,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"date": {
										Type:         schema.TypeString,
										Optional:     true,
										ValidateFunc: validateS3BucketLifecycleTimestamp,
									},
									"days": {
										Type:         schema.TypeInt,
										Optional:     true,
										ValidateFunc: validation.IntAtLeast(0),
									},
									"storage_class": {
										Type:         schema.TypeString,
										Required:     true,
										ValidateFunc: validation.StringInSlice(storageClassSet, false),
									},
								},
							},
						},
						"noncurrent_version_transition": {
							Type:     schema.TypeSet,
							Optional: true,
							Set:      transitionHash,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"days": {
										Type:         schema.TypeInt,
										Optional:     true,
										ValidateFunc: validation.IntAtLeast(0),
									},
									"storage_class": {
										Type:         schema.TypeString,
										Required:     true,
										ValidateFunc: validation.StringInSlice(storageClassSet, false),
									},
								},
							},
						},
					},
				},
			},

			"force_destroy": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},

			"server_side_encryption_configuration": {
				Type:     schema.TypeList,
				MaxItems: 1,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"rule": {
							Type:     schema.TypeList,
							MaxItems: 1,
							Required: true,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"apply_server_side_encryption_by_default": {
										Type:     schema.TypeList,
										MaxItems: 1,
										Required: true,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"kms_master_key_id": {
													Type:     schema.TypeString,
													Required: true,
												},
												"sse_algorithm": {
													Type:     schema.TypeString,
													Required: true,
													ValidateFunc: validation.StringInSlice([]string{
														s3.ServerSideEncryptionAwsKms,
													}, false),
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},

			// These fields use extended API and requires IAM token
			// to be set in order to operate.
			"default_storage_class": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
			},

			"folder_id": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
				ForceNew: true,
			},

			"max_size": {
				Type:     schema.TypeInt,
				Optional: true,
			},

			"anonymous_access_flags": {
				Type:     schema.TypeSet,
				Optional: true,
				Computed: true,
				MaxItems: 1,
				Set:      storageBucketS3SetFunc("list", "read", "config_read"),
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"list": {
							Type:     schema.TypeBool,
							Optional: true,
						},
						"read": {
							Type:     schema.TypeBool,
							Optional: true,
						},
						"config_read": {
							Type:     schema.TypeBool,
							Optional: true,
						},
					},
				},
			},

			"https": {
				Type:     schema.TypeSet,
				Optional: true,
				MaxItems: 1,
				Set:      storageBucketS3SetFunc("certificate_id"),
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"certificate_id": {
							Type:     schema.TypeString,
							Required: true,
						},
					},
				},
			},
			"tags": tagsSchema(),
		},
	}
}

func tagsSchema() *schema.Schema {
	return &schema.Schema{
		Type:     schema.TypeMap,
		Optional: true,
		Elem:     &schema.Schema{Type: schema.TypeString},
	}
}

const (
	bucketACLOwnerFullControl = "bucket-owner-full-control"
	bucketACLPublicRead       = s3.BucketCannedACLPublicRead
	bucketACLPublicReadWrite  = s3.BucketCannedACLPublicReadWrite
	bucketACLAuthRead         = s3.BucketCannedACLAuthenticatedRead
	bucketACLPrivate          = s3.BucketCannedACLPrivate
)

var bucketACLAllowedValues = []string{
	bucketACLOwnerFullControl,
	bucketACLPublicRead,
	bucketACLPublicReadWrite,
	bucketACLAuthRead,
	bucketACLPrivate,
}

func resourceYandexStorageBucketCreateBySDK(d *schema.ResourceData, meta interface{}) error {
	mapACL := func(acl string) (*storagepb.ACL, error) {
		baseACL := &storagepb.ACL{}
		switch acl {
		case bucketACLPublicRead:
			baseACL.Grants = []*storagepb.ACL_Grant{{
				Permission: storagepb.ACL_Grant_PERMISSION_READ,
				GrantType:  storagepb.ACL_Grant_GRANT_TYPE_ALL_USERS,
			}}
		case bucketACLPublicReadWrite:
			baseACL.Grants = []*storagepb.ACL_Grant{{
				Permission: storagepb.ACL_Grant_PERMISSION_READ,
				GrantType:  storagepb.ACL_Grant_GRANT_TYPE_ALL_USERS,
			}, {
				Permission: storagepb.ACL_Grant_PERMISSION_READ,
				GrantType:  storagepb.ACL_Grant_GRANT_TYPE_ALL_USERS,
			}}
		case bucketACLAuthRead:
			baseACL.Grants = []*storagepb.ACL_Grant{{
				Permission: storagepb.ACL_Grant_PERMISSION_READ,
				GrantType:  storagepb.ACL_Grant_GRANT_TYPE_ALL_AUTHENTICATED_USERS,
			}}
		case bucketACLPrivate,
			bucketACLOwnerFullControl:
			baseACL.Grants = []*storagepb.ACL_Grant{}
		}

		return baseACL, nil
	}

	bucket := d.Get("bucket").(string)
	folderID := d.Get("folder_id").(string)
	acl := d.Get("acl").(string)
	maxSize := d.Get("max_size").(int)
	defaultStorageClass := d.Get("default_storage_class").(string)
	aaf := getAnonymousAccessFlagsSDK(d.Get("anonymous_access_flags"))

	request := &storagepb.CreateBucketRequest{
		Name:                 bucket,
		FolderId:             folderID,
		AnonymousAccessFlags: aaf,
		MaxSize:              int64(maxSize),
		DefaultStorageClass:  defaultStorageClass,
	}

	var err error
	request.Acl, err = mapACL(acl)
	if err != nil {
		return fmt.Errorf("mapping acl: %w", err)
	}

	config := meta.(*Config)
	ctx := config.Context()

	log.Printf("[INFO] Creating Storage S3 bucket using sdk: %s", protojson.Format(request))

	bucketAPI := config.sdk.StorageAPI().Bucket()
	op, err := bucketAPI.Create(ctx, request)
	err = waitOperation(ctx, config, op, err)
	if err != nil {
		log.Printf("[ERROR] Unable to create S3 bucket using sdk: %v", err)

		return err
	}

	responseBucket := &storagepb.Bucket{}
	err = op.GetResponse().UnmarshalTo(responseBucket)
	if err != nil {
		log.Printf("[ERROR] Returned message is not a bucket: %v", err)

		return err
	}

	log.Printf("[INFO] Created Storage S3 bucket: %s", protojson.Format(responseBucket))

	return nil
}

func resourceYandexStorageBucketCreateByS3Client(d *schema.ResourceData, meta interface{}) error {
	bucket := d.Get("bucket").(string)
	var acl string
	if aclValue, ok := d.GetOk("acl"); ok {
		acl = aclValue.(string)
	} else {
		acl = bucketACLPrivate
	}

	config := meta.(*Config)
	ctx := config.Context()

	s3Client, err := getS3Client(d, config)
	if err != nil {
		return fmt.Errorf("error getting storage client: %s", err)
	}

	return resource.RetryContext(ctx, 5*time.Minute, func() *resource.RetryError {
		log.Printf("[INFO] Trying to create new Storage S3 Bucket: %q, ACL: %q", bucket, acl)

		_, err := s3Client.CreateBucket(&s3.CreateBucketInput{
			Bucket: aws.String(bucket),
			ACL:    aws.String(acl),
		})
		if awsErr, ok := err.(awserr.Error); ok && (awsErr.Code() == "OperationAborted" ||
			awsErr.Code() == "AccessDenied" || awsErr.Code() == "Forbidden") {
			log.Printf("[WARN] Got an error while trying to create Storage S3 Bucket %s: %s", bucket, err)
			return resource.RetryableError(
				fmt.Errorf("error creating Storage S3 Bucket %s, retrying: %s", bucket, err))
		}
		if err != nil {
			log.Printf("[ERROR] Got an error while trying to create Storage Bucket %s: %s", bucket, err)
			return resource.NonRetryableError(err)
		}

		log.Printf("[INFO] Created new Storage S3 Bucket: %q, ACL: %q", bucket, acl)
		return nil
	})
}

func resourceYandexStorageBucketCreate(d *schema.ResourceData, meta interface{}) error {
	// Get the bucket and acl
	var bucket string
	if v, ok := d.GetOk("bucket"); ok {
		bucket = v.(string)
	} else if v, ok := d.GetOk("bucket_prefix"); ok {
		bucket = resource.PrefixedUniqueId(v.(string))
	} else {
		bucket = resource.UniqueId()
	}

	if err := validateS3BucketName(bucket); err != nil {
		return fmt.Errorf("error validating Storage Bucket name: %s", err)
	}

	d.Set("bucket", bucket)

	var err error
	if folderID, ok := d.Get("folder_id").(string); ok && folderID != "" {
		err = resourceYandexStorageBucketCreateBySDK(d, meta)
	} else {
		err = resourceYandexStorageBucketCreateByS3Client(d, meta)
	}
	if err != nil {
		return fmt.Errorf("error creating Storage S3 Bucket: %s", err)
	}

	d.SetId(bucket)

	return resourceYandexStorageBucketUpdate(d, meta)
}

func resourceYandexStorageRequireExternalSDK(d *schema.ResourceData) bool {
	value, ok := d.GetOk("folder_id")
	if !ok {
		return false
	}

	folderID, ok := value.(string)
	if !ok {
		return false
	}

	return folderID != ""
}

func resourceYandexStorageBucketUpdate(d *schema.ResourceData, meta interface{}) error {
	err := resourceYandexStorageBucketUpdateBasic(d, meta)
	if err != nil {
		return err
	}

	err = resourceYandexStorageBucketUpdateExtended(d, meta)
	if err != nil {
		return err
	}

	return resourceYandexStorageBucketRead(d, meta)
}

func resourceYandexStorageBucketUpdateBasic(d *schema.ResourceData, meta interface{}) error {
	config := meta.(*Config)
	s3Client, err := getS3Client(d, config)
	if err != nil {
		return fmt.Errorf("error getting storage client: %s", err)
	}

	type property struct {
		name          string
		updateHandler func(*s3.S3, *schema.ResourceData) error
	}
	resourceProperties := []property{
		{"policy", resourceYandexStorageBucketPolicyUpdate},
		{"cors_rule", resourceYandexStorageBucketCORSUpdate},
		{"website", resourceYandexStorageBucketWebsiteUpdate},
		{"versioning", resourceYandexStorageBucketVersioningUpdate},
		{"acl", resourceYandexStorageBucketACLUpdate},
		{"grant", resourceYandexStorageBucketGrantsUpdate},
		{"logging", resourceYandexStorageBucketLoggingUpdate},
		{"lifecycle_rule", resourceYandexStorageBucketLifecycleUpdate},
		{"server_side_encryption_configuration", resourceYandexStorageBucketServerSideEncryptionConfigurationUpdate},
		{"object_lock_configuration", resourceYandexStorageBucketObjectLockConfigurationUpdate},
		{"tags", resourceYandexStorageBucketTagsUpdate},
	}

	for _, property := range resourceProperties {
		if !d.HasChange(property.name) {
			continue
		}

		if property.name == "acl" && d.IsNewResource() {
			continue
		}

		err := property.updateHandler(s3Client, d)
		if err != nil {
			return fmt.Errorf("handling %s: %w", property.name, err)
		}
	}

	return nil
}

func resourceYandexStorageBucketUpdateExtended(d *schema.ResourceData, meta interface{}) (err error) {
	if d.Id() == "" {
		// bucket has been deleted, skipping
		return nil
	}

	bucket := d.Get("bucket").(string)
	bucketUpdateRequest := &storagepb.UpdateBucketRequest{
		Name: bucket,
	}
	paths := make([]string, 0, 3)

	createdBySDK := resourceYandexStorageRequireExternalSDK(d)
	handleChange := func(property string, f func(value interface{})) {
		switch {
		// If this bucket is a new resource and we created it
		// by our SDK it means we've already set all parameters
		// to its values.
		case d.IsNewResource() && createdBySDK:
			fallthrough
		case !d.HasChange(property):
			return
		}

		log.Printf("[DEBUG] property %q is going to be updated", property)

		value := d.Get(property)
		f(value)

		paths = append(paths, property)
	}

	changeHandlers := map[string]func(value interface{}){
		"default_storage_class": func(value interface{}) {
			bucketUpdateRequest.SetDefaultStorageClass(value.(string))
		},
		"max_size": func(value interface{}) {
			bucketUpdateRequest.SetMaxSize(int64(value.(int)))
		},
		"anonymous_access_flags": func(value interface{}) {
			bucketUpdateRequest.AnonymousAccessFlags = getAnonymousAccessFlagsSDK(value)
		},
	}

	for field, handler := range changeHandlers {
		handleChange(field, handler)
	}

	config := meta.(*Config)
	ctx, cancel := config.ContextWithTimeout(d.Timeout(schema.TimeoutUpdate))
	defer cancel()

	bucketAPI := config.sdk.StorageAPI().Bucket()

	if len(paths) > 0 {
		bucketUpdateRequest.UpdateMask, err = fieldmaskpb.New(bucketUpdateRequest, paths...)
		if err != nil {
			return fmt.Errorf("constructing field mask: %w", err)
		}

		log.Printf("[INFO] updating S3 bucket extended parameters: %s", protojson.Format(bucketUpdateRequest))

		op, err := bucketAPI.Update(ctx, bucketUpdateRequest)
		err = waitOperation(ctx, config, op, err)
		if err != nil {
			if handleS3BucketNotFoundError(d, err) {
				return nil
			}

			log.Printf("[WARN] Storage api error updating S3 bucket extended parameters: %v", err)

			return err
		}

		if opErr := op.GetError(); opErr != nil {
			log.Printf("[WARN] Operation ended with error: %s", protojson.Format(opErr))

			return status.Error(codes.Code(opErr.Code), opErr.Message)
		}

		log.Printf("[INFO] updated S3 bucket extended parameters: %s", protojson.Format(op.GetResponse()))
	}

	if !d.HasChange("https") {
		return nil
	}

	log.Println("[DEBUG] updating S3 bucket https configuration")

	schemaSet := d.Get("https").(*schema.Set)
	if schemaSet.Len() > 0 {
		httpsUpdateRequest := &storagepb.SetBucketHTTPSConfigRequest{
			Name: bucket,
		}

		params := schemaSet.List()[0].(map[string]interface{})
		httpsUpdateRequest.Params = &storagepb.SetBucketHTTPSConfigRequest_CertificateManager{
			CertificateManager: &storagepb.CertificateManagerHTTPSConfigParams{
				CertificateId: params["certificate_id"].(string),
			},
		}

		log.Printf("[INFO] updating S3 bucket https config: %s", protojson.Format(httpsUpdateRequest))
		op, err := bucketAPI.SetHTTPSConfig(ctx, httpsUpdateRequest)
		err = waitOperation(ctx, config, op, err)
		if err != nil {
			if handleS3BucketNotFoundError(d, err) {
				return nil
			}

			log.Printf("[WARN] Storage api updating S3 bucket https config: %v", err)

			return err
		}

		if opErr := op.GetError(); opErr != nil {
			log.Printf("[WARN] Operation ended with error: %s", protojson.Format(opErr))

			return status.Error(codes.Code(opErr.Code), opErr.Message)
		}

		log.Printf("[INFO] updated S3 bucket https config: %s", protojson.Format(op.GetResponse()))

		return nil
	}

	httpsDeleteRequest := &storagepb.DeleteBucketHTTPSConfigRequest{
		Name: bucket,
	}

	log.Printf("[INFO] deleting S3 bucket https config: %s", protojson.Format(httpsDeleteRequest))
	op, err := bucketAPI.DeleteHTTPSConfig(ctx, httpsDeleteRequest)
	err = waitOperation(ctx, config, op, err)
	if err != nil {
		if handleS3BucketNotFoundError(d, err) {
			return nil
		}

		log.Printf("[WARN] Storage api deleting S3 bucket https config: %v", err)

		return err
	}

	if opErr := op.GetError(); opErr != nil {
		log.Printf("[WARN] Operation ended with error: %s", protojson.Format(opErr))

		return status.Error(codes.Code(opErr.Code), opErr.Message)
	}
	log.Printf("[INFO] deleted S3 bucket https config: %s", protojson.Format(op.GetResponse()))

	return nil
}

func resourceYandexStorageBucketRead(d *schema.ResourceData, meta interface{}) error {
	err := resourceYandexStorageBucketReadBasic(d, meta)
	if err != nil {
		return err
	}

	err = resourceYandexStorageBucketReadExtended(d, meta)
	if err != nil {
		log.Printf("[WARN] Got an error reading Storage Bucket's extended properties: %s", err)
	}

	return nil
}

func resourceYandexStorageBucketReadBasic(d *schema.ResourceData, meta interface{}) error {
	config := meta.(*Config)
	s3Client, err := getS3Client(d, config)

	bucketAWS := aws.String(d.Id())

	if err != nil {
		return fmt.Errorf("error getting storage client: %s", err)
	}

	resp, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.HeadBucket(&s3.HeadBucketInput{
			Bucket: bucketAWS,
		})
	})
	if err != nil {
		if handleS3BucketNotFoundError(d, err) {
			return nil
		}
		return fmt.Errorf("error reading Storage Bucket (%s): %s", d.Id(), err)
	}
	log.Printf("[DEBUG] Storage head bucket output: %#v", resp)

	if _, ok := d.GetOk("bucket"); !ok {
		d.Set("bucket", d.Id())
	}

	domainName, err := bucketDomainName(d.Get("bucket").(string), config.StorageEndpoint)
	if err != nil {
		return fmt.Errorf("error getting bucket domain name: %s", err)
	}
	d.Set("bucket_domain_name", domainName)

	// Read the policy
	pol, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetBucketPolicy(&s3.GetBucketPolicyInput{
			Bucket: bucketAWS,
		})
	})
	log.Printf("[DEBUG] S3 bucket: %s, read policy: %v", d.Id(), pol)
	switch {
	case err == nil:
		v := pol.(*s3.GetBucketPolicyOutput).Policy
		if v == nil {
			if err := d.Set("policy", ""); err != nil {
				return fmt.Errorf("error setting policy: %s", err)
			}
		} else {
			policy, err := NormalizeJsonString(aws.StringValue(v))
			if err != nil {
				return fmt.Errorf("policy contains an invalid JSON: %s", err)
			}
			if err := d.Set("policy", policy); err != nil {
				return fmt.Errorf("error setting policy: %s", err)
			}
		}
	case isAWSErr(err, "NoSuchBucketPolicy", ""):
		d.Set("policy", "")
	case isAWSErr(err, "AccessDenied", ""):
		log.Printf("[WARN] Got an error while trying to read Storage Bucket (%s) Policy: %s", d.Id(), err)
		d.Set("policy", nil)
	default:
		return fmt.Errorf("error getting current policy: %s", err)
	}

	corsResponse, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetBucketCors(&s3.GetBucketCorsInput{
			Bucket: bucketAWS,
		})
	})
	if err != nil && !isAWSErr(err, "NoSuchCORSConfiguration", "") {
		if handleS3BucketNotFoundError(d, err) {
			return nil
		}
		return fmt.Errorf("error getting Storage Bucket CORS configuration: %s", err)
	}

	corsRules := make([]map[string]interface{}, 0)
	if cors, ok := corsResponse.(*s3.GetBucketCorsOutput); ok && len(cors.CORSRules) > 0 {
		log.Printf("[DEBUG] Storage get bucket CORS output: %#v", cors)

		corsRules = make([]map[string]interface{}, 0, len(cors.CORSRules))
		for _, ruleObject := range cors.CORSRules {
			rule := make(map[string]interface{})
			rule["allowed_headers"] = flattenStringList(ruleObject.AllowedHeaders)
			rule["allowed_methods"] = flattenStringList(ruleObject.AllowedMethods)
			rule["allowed_origins"] = flattenStringList(ruleObject.AllowedOrigins)
			// Both the "ExposeHeaders" and "MaxAgeSeconds" might not be set.
			if ruleObject.ExposeHeaders != nil {
				rule["expose_headers"] = flattenStringList(ruleObject.ExposeHeaders)
			}
			if ruleObject.MaxAgeSeconds != nil {
				rule["max_age_seconds"] = int(*ruleObject.MaxAgeSeconds)
			}
			corsRules = append(corsRules, rule)
		}
	}
	if err := d.Set("cors_rule", corsRules); err != nil {
		return fmt.Errorf("error setting cors_rule: %s", err)
	}

	// Read the website configuration
	wsResponse, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetBucketWebsite(&s3.GetBucketWebsiteInput{
			Bucket: bucketAWS,
		})
	})
	if err != nil && !isAWSErr(err, "NotImplemented", "") && !isAWSErr(err, "NoSuchWebsiteConfiguration", "") {
		if handleS3BucketNotFoundError(d, err) {
			return nil
		}
		return fmt.Errorf("error getting Storage Bucket website configuration: %s", err)
	}

	websites := make([]map[string]interface{}, 0, 1)
	if ws, ok := wsResponse.(*s3.GetBucketWebsiteOutput); ok {
		log.Printf("[DEBUG] Storage get bucket website output: %#v", ws)

		w := make(map[string]interface{})

		if v := ws.IndexDocument; v != nil {
			w["index_document"] = *v.Suffix
		}

		if v := ws.ErrorDocument; v != nil {
			w["error_document"] = *v.Key
		}

		if v := ws.RedirectAllRequestsTo; v != nil {
			if v.Protocol == nil {
				w["redirect_all_requests_to"] = aws.StringValue(v.HostName)
			} else {
				var host string
				var path string
				var query string
				parsedHostName, err := url.Parse(aws.StringValue(v.HostName))
				if err == nil {
					host = parsedHostName.Host
					path = parsedHostName.Path
					query = parsedHostName.RawQuery
				} else {
					host = aws.StringValue(v.HostName)
					path = ""
				}

				w["redirect_all_requests_to"] = (&url.URL{
					Host:     host,
					Path:     path,
					Scheme:   aws.StringValue(v.Protocol),
					RawQuery: query,
				}).String()
			}
		}

		if v := ws.RoutingRules; v != nil {
			rr, err := normalizeRoutingRules(v)
			if err != nil {
				return fmt.Errorf("Error while marshaling routing rules: %s", err)
			}
			w["routing_rules"] = rr
		}

		// We have special handling for the website configuration,
		// so only add the configuration if there is any
		if len(w) > 0 {
			websites = append(websites, w)
		}
	}
	if err := d.Set("website", websites); err != nil {
		return fmt.Errorf("error setting website: %s", err)
	}

	// Add website_endpoint as an attribute
	websiteEndpoint, err := websiteEndpoint(s3Client, d)
	if err != nil {
		return err
	}
	if websiteEndpoint != nil {
		if err := d.Set("website_endpoint", websiteEndpoint.Endpoint); err != nil {
			return fmt.Errorf("error setting website_endpoint: %s", err)
		}
		if err := d.Set("website_domain", websiteEndpoint.Domain); err != nil {
			return fmt.Errorf("error setting website_domain: %s", err)
		}
	}

	apResponse, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetBucketAcl(&s3.GetBucketAclInput{
			Bucket: bucketAWS,
		})
	})

	if !d.IsNewResource() && isAWSErr(err, s3.ErrCodeNoSuchBucket, "") {
		log.Printf("[WARN] requested bucket not found, deleting")
		d.SetId("")
		return nil
	}

	if err != nil {
		// Ignore access denied error, when reading ACL for bucket.
		if awsErr, ok := err.(awserr.Error); ok && (awsErr.Code() == "AccessDenied" || awsErr.Code() == "Forbidden") {
			log.Printf("[WARN] Got an error while trying to read Storage Bucket (%s) ACL: %s", d.Id(), err)

			if err := d.Set("grant", nil); err != nil {
				return fmt.Errorf("error resetting Storage Bucket `grant` %s", err)
			}

			return nil
		}

		return fmt.Errorf("error getting Storage Bucket (%s) ACL: %s", d.Id(), err)
	} else {
		log.Printf("[DEBUG] getting storage: %s, read ACL grants policy: %+v", d.Id(), apResponse)
		grants := flattenGrants(apResponse.(*s3.GetBucketAclOutput))
		if err := d.Set("grant", schema.NewSet(grantHash, grants)); err != nil {
			return fmt.Errorf("error setting Storage Bucket `grant` %s", err)
		}
	}

	// Read the versioning configuration

	versioningResponse, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetBucketVersioning(&s3.GetBucketVersioningInput{
			Bucket: bucketAWS,
		})
	})
	if err != nil {
		return err
	}

	vcl := make([]map[string]interface{}, 0, 1)
	if versioning, ok := versioningResponse.(*s3.GetBucketVersioningOutput); ok {
		vc := make(map[string]interface{})
		if versioning.Status != nil && aws.StringValue(versioning.Status) == s3.BucketVersioningStatusEnabled {
			vc["enabled"] = true
		} else {
			vc["enabled"] = false
		}

		vcl = append(vcl, vc)
	}
	if err := d.Set("versioning", vcl); err != nil {
		return fmt.Errorf("error setting versioning: %s", err)
	}

	// Read the Object Lock Configuration
	objectLockConfigResponse, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetObjectLockConfiguration(&s3.GetObjectLockConfigurationInput{
			Bucket: bucketAWS,
		})
	})
	if err != nil &&
		(!isAWSErr(err, "ObjectLockConfigurationNotFoundError", "") && !isAWSErr(err, "AccessDenied", "")) {
		log.Printf("[WARN] Got an error while trying to read Storage Bucket (%s) ObjectLockConfiguration: %s", d.Id(), err)
		return err
	} else {
		log.Printf("[DEBUG] Got an error while trying to read Storage Bucket (%s) ObjectLockConfigurationt: %s", d.Id(), err)
	}

	var olcl []map[string]interface{}
	objectLockConfig, ok := objectLockConfigResponse.(*s3.GetObjectLockConfigurationOutput)
	if err == nil && ok && objectLockConfig.ObjectLockConfiguration != nil {
		log.Printf("[DEBUG] Storage get bucket object lock config output: %#v", objectLockConfig)
		olcl = make([]map[string]interface{}, 0, 1)
		olc := make(map[string]interface{})

		enabled := objectLockConfig.ObjectLockConfiguration.ObjectLockEnabled
		rule := objectLockConfig.ObjectLockConfiguration.Rule

		if aws.StringValue(enabled) != "" {
			olc["object_lock_enabled"] = aws.StringValue(enabled)
		}

		if rule != nil {
			rt := make(map[string]interface{}, 2)
			defaultRetention := rule.DefaultRetention

			rt["mode"] = aws.StringValue(defaultRetention.Mode)
			if defaultRetention.Days != nil {
				rt["days"] = aws.Int64Value(defaultRetention.Days)
			}
			if defaultRetention.Years != nil {
				rt["years"] = aws.Int64Value(defaultRetention.Years)
			}

			dr := make(map[string]interface{})
			dr["default_retention"] = []interface{}{rt}
			olc["rule"] = []interface{}{dr}
		}

		olcl = append(olcl, olc)
	}
	if err := d.Set("object_lock_configuration", olcl); err != nil {
		return fmt.Errorf("error setting object lock configuration: %s", err)
	}

	// Read the logging configuration
	loggingResponse, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetBucketLogging(&s3.GetBucketLoggingInput{
			Bucket: bucketAWS,
		})
	})

	if err != nil {
		return fmt.Errorf("error getting S3 Bucket logging: %s", err)
	}

	lcl := make([]map[string]interface{}, 0, 1)
	if logging, ok := loggingResponse.(*s3.GetBucketLoggingOutput); ok && logging.LoggingEnabled != nil {
		v := logging.LoggingEnabled
		lc := make(map[string]interface{})
		if aws.StringValue(v.TargetBucket) != "" {
			lc["target_bucket"] = aws.StringValue(v.TargetBucket)
		}
		if aws.StringValue(v.TargetPrefix) != "" {
			lc["target_prefix"] = aws.StringValue(v.TargetPrefix)
		}
		lcl = append(lcl, lc)
	}
	if err := d.Set("logging", lcl); err != nil {
		return fmt.Errorf("error setting logging: %s", err)
	}

	// Read the lifecycle configuration
	lifecycleResponse, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetBucketLifecycleConfiguration(&s3.GetBucketLifecycleConfigurationInput{
			Bucket: bucketAWS,
		})
	})
	if err != nil && !isAWSErr(err, "NoSuchLifecycleConfiguration", "") {
		return err
	}

	lifecycleRules := make([]map[string]interface{}, 0)
	if lifecycle, ok := lifecycleResponse.(*s3.GetBucketLifecycleConfigurationOutput); ok && len(lifecycle.Rules) > 0 {
		lifecycleRules = make([]map[string]interface{}, 0, len(lifecycle.Rules))

		for _, lifecycleRule := range lifecycle.Rules {
			log.Printf("[DEBUG] S3 bucket: %s, read lifecycle rule: %v", d.Id(), lifecycleRule)
			rule := make(map[string]interface{})

			// ID
			if lifecycleRule.ID != nil && aws.StringValue(lifecycleRule.ID) != "" {
				rule["id"] = aws.StringValue(lifecycleRule.ID)
			}
			filter := lifecycleRule.Filter
			if filter != nil {
				if filter.And != nil {
					// Prefix
					if filter.And.Prefix != nil && aws.StringValue(filter.And.Prefix) != "" {
						rule["prefix"] = aws.StringValue(filter.And.Prefix)
					}
				} else {
					// Prefix
					if filter.Prefix != nil && aws.StringValue(filter.Prefix) != "" {
						rule["prefix"] = aws.StringValue(filter.Prefix)
					}
				}
			}

			// Enabled
			if lifecycleRule.Status != nil {
				if aws.StringValue(lifecycleRule.Status) == s3.ExpirationStatusEnabled {
					rule["enabled"] = true
				} else {
					rule["enabled"] = false
				}
			}

			// AbortIncompleteMultipartUploadDays
			if lifecycleRule.AbortIncompleteMultipartUpload != nil {
				if lifecycleRule.AbortIncompleteMultipartUpload.DaysAfterInitiation != nil {
					rule["abort_incomplete_multipart_upload_days"] = int(aws.Int64Value(lifecycleRule.AbortIncompleteMultipartUpload.DaysAfterInitiation))
				}
			}

			// expiration
			if lifecycleRule.Expiration != nil {
				e := make(map[string]interface{})
				if lifecycleRule.Expiration.Date != nil {
					e["date"] = (aws.TimeValue(lifecycleRule.Expiration.Date)).Format("2006-01-02")
				}
				if lifecycleRule.Expiration.Days != nil {
					e["days"] = int(aws.Int64Value(lifecycleRule.Expiration.Days))
				}
				if lifecycleRule.Expiration.ExpiredObjectDeleteMarker != nil {
					e["expired_object_delete_marker"] = aws.BoolValue(lifecycleRule.Expiration.ExpiredObjectDeleteMarker)
				}
				rule["expiration"] = []interface{}{e}
			}
			// noncurrent_version_expiration
			if lifecycleRule.NoncurrentVersionExpiration != nil {
				e := make(map[string]interface{})
				if lifecycleRule.NoncurrentVersionExpiration.NoncurrentDays != nil {
					e["days"] = int(aws.Int64Value(lifecycleRule.NoncurrentVersionExpiration.NoncurrentDays))
				}
				rule["noncurrent_version_expiration"] = []interface{}{e}
			}
			//// transition
			if len(lifecycleRule.Transitions) > 0 {
				transitions := make([]interface{}, 0, len(lifecycleRule.Transitions))
				for _, v := range lifecycleRule.Transitions {
					t := make(map[string]interface{})
					if v.Date != nil {
						t["date"] = (aws.TimeValue(v.Date)).Format("2006-01-02")
					}
					if v.Days != nil {
						t["days"] = int(aws.Int64Value(v.Days))
					}
					if v.StorageClass != nil {
						t["storage_class"] = aws.StringValue(v.StorageClass)
					}
					transitions = append(transitions, t)
				}
				rule["transition"] = schema.NewSet(transitionHash, transitions)
			}
			// noncurrent_version_transition
			if len(lifecycleRule.NoncurrentVersionTransitions) > 0 {
				transitions := make([]interface{}, 0, len(lifecycleRule.NoncurrentVersionTransitions))
				for _, v := range lifecycleRule.NoncurrentVersionTransitions {
					t := make(map[string]interface{})
					if v.NoncurrentDays != nil {
						t["days"] = int(aws.Int64Value(v.NoncurrentDays))
					}
					if v.StorageClass != nil {
						t["storage_class"] = aws.StringValue(v.StorageClass)
					}
					transitions = append(transitions, t)
				}
				rule["noncurrent_version_transition"] = schema.NewSet(transitionHash, transitions)
			}

			lifecycleRules = append(lifecycleRules, rule)
		}
	}
	if err := d.Set("lifecycle_rule", lifecycleRules); err != nil {
		return fmt.Errorf("error setting lifecycle_rule: %s", err)
	}

	// Read the bucket server side encryption configuration

	encryptionResponse, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetBucketEncryption(&s3.GetBucketEncryptionInput{
			Bucket: bucketAWS,
		})
	})
	if err != nil && !isAWSErr(err, "ServerSideEncryptionConfigurationNotFoundError", "encryption configuration was not found") {
		return fmt.Errorf("error getting S3 Bucket encryption: %w", err)
	}

	serverSideEncryptionConfiguration := make([]map[string]interface{}, 0)
	if encryption, ok := encryptionResponse.(*s3.GetBucketEncryptionOutput); ok && encryption.ServerSideEncryptionConfiguration != nil {
		serverSideEncryptionConfiguration = flattenS3ServerSideEncryptionConfiguration(encryption.ServerSideEncryptionConfiguration)
	}
	if err := d.Set("server_side_encryption_configuration", serverSideEncryptionConfiguration); err != nil {
		return fmt.Errorf("error setting server_side_encryption_configuration: %s", err)
	}

	getBucketTagging, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.GetBucketTagging(&s3.GetBucketTaggingInput{
			Bucket: bucketAWS,
		})
	})
	if err != nil {
		return fmt.Errorf("error getting S3 Bucket tags: %w", err)
	}

	tags := getBucketTagging.(*s3.GetBucketTaggingOutput)
	tagsNormalized := storageBucketTaggingNormalize(tags.TagSet)
	err = d.Set("tags", tagsNormalized)
	if err != nil {
		return fmt.Errorf("error setting S3 Bucket tags: %w", err)
	}

	return nil
}

func resourceYandexStorageBucketReadExtended(d *schema.ResourceData, meta interface{}) error {
	if d.Id() == "" {
		// bucket has been deleted, skipping read
		return nil
	}

	config := meta.(*Config)
	bucketAPI := config.sdk.StorageAPI().Bucket()

	name := d.Get("bucket").(string)

	ctx, cancel := config.ContextWithTimeout(d.Timeout(schema.TimeoutRead))
	defer cancel()

	log.Println("[DEBUG] Getting S3 bucket extended parameters")

	bucket, err := bucketAPI.Get(ctx, &storagepb.GetBucketRequest{
		Name: name,
		View: storagepb.GetBucketRequest_VIEW_FULL,
	})
	if err != nil {
		if handleS3BucketNotFoundError(d, err) {
			return nil
		}

		log.Printf("[WARN] Storage api getting S3 bucket extended parameters: %v", err)

		return err
	}

	log.Printf("[DEBUG] Bucket %s", protojson.Format(bucket))

	d.Set("default_storage_class", bucket.GetDefaultStorageClass())
	d.Set("folder_id", bucket.GetFolderId())
	d.Set("max_size", bucket.GetMaxSize())

	aafValue := make([]map[string]interface{}, 0)
	if aaf := bucket.AnonymousAccessFlags; aaf != nil {
		flatten := map[string]interface{}{}
		if value := aaf.List; value != nil {
			flatten["list"] = value.Value
		}
		if value := aaf.Read; value != nil {
			flatten["read"] = value.Value
		}
		if value := aaf.ConfigRead; value != nil {
			flatten["config_read"] = value.Value
		}

		aafValue = append(aafValue, flatten)
	}

	log.Printf("[DEBUG] setting anonymous access flags: %v", aafValue)
	if len(aafValue) == 0 {
		d.Set("anonymous_access_flags", nil)
	} else {
		d.Set("anonymous_access_flags", aafValue)
	}

	log.Println("[DEBUG] trying to get S3 bucket https config")

	https, err := bucketAPI.GetHTTPSConfig(ctx, &storagepb.GetBucketHTTPSConfigRequest{
		Name: name,
	})
	switch {
	case err == nil:
		// continue
	case isStatusWithCode(err, codes.NotFound),
		isStatusWithCode(err, codes.PermissionDenied):
		log.Printf("[INFO] Storage api got minor error getting S3 bucket https config %v", err)
		d.Set("https", nil)

		return nil
	default:
		log.Printf("[WARN] Storage api error getting S3 bucket https config %v", err)

		return err
	}

	log.Printf("[DEBUG] S3 bucket https config: %s", protojson.Format(https))

	if https.SourceType == storagepb.HTTPSConfig_SOURCE_TYPE_MANAGED_BY_CERTIFICATE_MANAGER {
		flatten := map[string]interface{}{
			"certificate_id": https.CertificateId,
		}

		result := []map[string]interface{}{flatten}

		err = d.Set("https", result)
		if err != nil {
			return fmt.Errorf("updating S3 bucket https config state: %w", err)
		}
	}

	return nil
}

func resourceYandexStorageBucketDelete(d *schema.ResourceData, meta interface{}) error {
	config := meta.(*Config)
	s3Client, err := getS3Client(d, config)
	if err != nil {
		return fmt.Errorf("error getting storage client: %s", err)
	}

	log.Printf("[DEBUG] Storage Delete Bucket: %s", d.Id())

	_, err = retryOnAwsCodes([]string{"AccessDenied", "Forbidden"}, func() (interface{}, error) {
		return s3Client.DeleteBucket(&s3.DeleteBucketInput{
			Bucket: aws.String(d.Id()),
		})
	})

	if isAWSErr(err, s3.ErrCodeNoSuchBucket, "") {
		return nil
	}

	if isAWSErr(err, "BucketNotEmpty", "") {
		if d.Get("force_destroy").(bool) {
			// bucket may have things delete them
			log.Printf("[DEBUG] Storage Bucket attempting to forceDestroy %+v", err)

			bucket := d.Get("bucket").(string)
			resp, err := s3Client.ListObjectVersions(
				&s3.ListObjectVersionsInput{
					Bucket: aws.String(bucket),
				},
			)

			if err != nil {
				return fmt.Errorf("error listing Storage Bucket object versions: %s", err)
			}

			objectsToDelete := make([]*s3.ObjectIdentifier, 0)

			if len(resp.DeleteMarkers) != 0 {
				for _, v := range resp.DeleteMarkers {
					objectsToDelete = append(objectsToDelete, &s3.ObjectIdentifier{
						Key:       v.Key,
						VersionId: v.VersionId,
					})
				}
			}

			if len(resp.Versions) != 0 {
				for _, v := range resp.Versions {
					objectsToDelete = append(objectsToDelete, &s3.ObjectIdentifier{
						Key:       v.Key,
						VersionId: v.VersionId,
					})
				}
			}

			params := &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &s3.Delete{
					Objects: objectsToDelete,
				},
			}

			_, err = s3Client.DeleteObjects(params)

			if err != nil {
				return fmt.Errorf("error force_destroy deleting Storage Bucket (%s): %s", d.Id(), err)
			}

			// this line recurses until all objects are deleted or an error is returned
			return resourceYandexStorageBucketDelete(d, meta)
		}
	}

	if err == nil {
		req := &s3.HeadBucketInput{
			Bucket: aws.String(d.Id()),
		}
		err = waitConditionStable(func() (bool, error) {
			_, err := s3Client.HeadBucket(req)
			if awsError, ok := err.(awserr.RequestFailure); ok && awsError.StatusCode() == 404 {
				return true, nil
			}
			return false, err
		})
	}

	if err != nil {
		return fmt.Errorf("error deleting Storage Bucket (%s): %s", d.Id(), err)
	}

	return nil
}

func resourceYandexStorageBucketCORSUpdate(s3Client *s3.S3, d *schema.ResourceData) error {
	bucket := d.Get("bucket").(string)
	rawCors := d.Get("cors_rule").([]interface{})

	if len(rawCors) == 0 {
		// Delete CORS
		log.Printf("[DEBUG] Storage Bucket: %s, delete CORS", bucket)

		_, err := retryFlakyS3Responses(func() (interface{}, error) {
			return s3Client.DeleteBucketCors(&s3.DeleteBucketCorsInput{
				Bucket: aws.String(bucket),
			})
		})
		if err == nil {
			err = waitCorsDeleted(s3Client, bucket)
		}
		if err != nil {
			return fmt.Errorf("error deleting storage CORS: %s", err)
		}
	} else {
		// Put CORS
		rules := make([]*s3.CORSRule, 0, len(rawCors))
		for _, cors := range rawCors {
			corsMap := cors.(map[string]interface{})
			r := &s3.CORSRule{}
			for k, v := range corsMap {
				log.Printf("[DEBUG] Storage Bucket: %s, put CORS: %#v, %#v", bucket, k, v)
				if k == "max_age_seconds" {
					r.MaxAgeSeconds = aws.Int64(int64(v.(int)))
				} else {
					vMap := make([]*string, len(v.([]interface{})))
					for i, vv := range v.([]interface{}) {
						var value string
						if str, ok := vv.(string); ok {
							value = str
						}
						vMap[i] = aws.String(value)
					}
					switch k {
					case "allowed_headers":
						r.AllowedHeaders = vMap
					case "allowed_methods":
						r.AllowedMethods = vMap
					case "allowed_origins":
						r.AllowedOrigins = vMap
					case "expose_headers":
						r.ExposeHeaders = vMap
					}
				}
			}
			rules = append(rules, r)
		}
		corsConfiguration := &s3.CORSConfiguration{
			CORSRules: rules,
		}
		corsInput := &s3.PutBucketCorsInput{
			Bucket:            aws.String(bucket),
			CORSConfiguration: corsConfiguration,
		}
		log.Printf("[DEBUG] Storage Bucket: %s, put CORS: %#v", bucket, corsInput)

		_, err := retryFlakyS3Responses(func() (interface{}, error) {
			return s3Client.PutBucketCors(corsInput)
		})
		if err == nil {
			err = waitCorsPut(s3Client, bucket, corsConfiguration)
		}
		if err != nil {
			return fmt.Errorf("error putting bucket CORS: %s", err)
		}
	}

	return nil
}

func resourceYandexStorageBucketWebsiteUpdate(s3Client *s3.S3, d *schema.ResourceData) error {
	ws := d.Get("website").([]interface{})

	if len(ws) == 0 {
		return resourceYandexStorageBucketWebsiteDelete(s3Client, d)
	}

	var w map[string]interface{}
	if ws[0] != nil {
		w = ws[0].(map[string]interface{})
	} else {
		w = make(map[string]interface{})
	}

	return resourceYandexStorageBucketWebsitePut(s3Client, d, w)
}

func resourceYandexStorageBucketWebsitePut(s3Client *s3.S3, d *schema.ResourceData, website map[string]interface{}) error {
	bucket := d.Get("bucket").(string)

	var indexDocument, errorDocument, redirectAllRequestsTo, routingRules string
	if v, ok := website["index_document"]; ok {
		indexDocument = v.(string)
	}
	if v, ok := website["error_document"]; ok {
		errorDocument = v.(string)
	}

	if v, ok := website["redirect_all_requests_to"]; ok {
		redirectAllRequestsTo = v.(string)
	}
	if v, ok := website["routing_rules"]; ok {
		routingRules = v.(string)
	}

	if indexDocument == "" && redirectAllRequestsTo == "" {
		return fmt.Errorf("must specify either index_document or redirect_all_requests_to")
	}

	websiteConfiguration := &s3.WebsiteConfiguration{}

	if indexDocument != "" {
		websiteConfiguration.IndexDocument = &s3.IndexDocument{Suffix: aws.String(indexDocument)}
	}

	if errorDocument != "" {
		websiteConfiguration.ErrorDocument = &s3.ErrorDocument{Key: aws.String(errorDocument)}
	}

	if redirectAllRequestsTo != "" {
		redirect, err := url.Parse(redirectAllRequestsTo)
		if err == nil && redirect.Scheme != "" {
			var redirectHostBuf bytes.Buffer
			redirectHostBuf.WriteString(redirect.Host)
			if redirect.Path != "" {
				redirectHostBuf.WriteString(redirect.Path)
			}
			if redirect.RawQuery != "" {
				redirectHostBuf.WriteString("?")
				redirectHostBuf.WriteString(redirect.RawQuery)
			}
			websiteConfiguration.RedirectAllRequestsTo = &s3.RedirectAllRequestsTo{HostName: aws.String(redirectHostBuf.String()), Protocol: aws.String(redirect.Scheme)}
		} else {
			websiteConfiguration.RedirectAllRequestsTo = &s3.RedirectAllRequestsTo{HostName: aws.String(redirectAllRequestsTo)}
		}
	}

	if routingRules != "" {
		var unmarshaledRules []*s3.RoutingRule
		if err := json.Unmarshal([]byte(routingRules), &unmarshaledRules); err != nil {
			return err
		}
		websiteConfiguration.RoutingRules = unmarshaledRules
	}

	putInput := &s3.PutBucketWebsiteInput{
		Bucket:               aws.String(bucket),
		WebsiteConfiguration: websiteConfiguration,
	}

	log.Printf("[DEBUG] Storage put bucket website: %#v", putInput)

	_, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.PutBucketWebsite(putInput)
	})
	if err == nil {
		err = waitWebsitePut(s3Client, bucket, websiteConfiguration)
	}
	if err != nil {
		return fmt.Errorf("error putting storage website: %s", err)
	}

	return nil
}

func resourceYandexStorageBucketWebsiteDelete(s3Client *s3.S3, d *schema.ResourceData) error {
	bucket := d.Get("bucket").(string)
	deleteInput := &s3.DeleteBucketWebsiteInput{Bucket: aws.String(bucket)}

	log.Printf("[DEBUG] Storage delete bucket website: %#v", deleteInput)

	_, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.DeleteBucketWebsite(deleteInput)
	})
	if err == nil {
		err = waitWebsiteDeleted(s3Client, bucket)
	}
	if err != nil {
		return fmt.Errorf("error deleting storage website: %s", err)
	}

	d.Set("website_endpoint", "")
	d.Set("website_domain", "")

	return nil
}

func websiteEndpoint(_ *s3.S3, d *schema.ResourceData) (*S3Website, error) {
	// If the bucket doesn't have a website configuration, return an empty
	// endpoint
	if _, ok := d.GetOk("website"); !ok {
		return nil, nil
	}

	bucket := d.Get("bucket").(string)

	return WebsiteEndpoint(bucket), nil
}

func WebsiteEndpoint(bucket string) *S3Website {
	domain := WebsiteDomainURL()
	return &S3Website{Endpoint: fmt.Sprintf("%s.%s", bucket, domain), Domain: domain}
}

func WebsiteDomainURL() string {
	return "website.yandexcloud.net"
}

func resourceYandexStorageBucketACLUpdate(s3Client *s3.S3, d *schema.ResourceData) error {
	acl := d.Get("acl").(string)
	if acl == "" {
		acl = bucketACLPrivate
	}

	bucket := d.Get("bucket").(string)

	i := &s3.PutBucketAclInput{
		Bucket: aws.String(bucket),
		ACL:    aws.String(acl),
	}
	log.Printf("[DEBUG] Storage put bucket ACL: %#v", i)

	_, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3Client.PutBucketAcl(i)
	})
	if err != nil {
		return fmt.Errorf("error putting Storage Bucket ACL: %s", err)
	}

	return nil
}

func resourceYandexStorageBucketVersioningUpdate(s3conn *s3.S3, d *schema.ResourceData) error {
	v := d.Get("versioning").([]interface{})
	bucket := d.Get("bucket").(string)
	vc := &s3.VersioningConfiguration{}

	if len(v) > 0 {
		c := v[0].(map[string]interface{})

		if c["enabled"].(bool) {
			vc.Status = aws.String(s3.BucketVersioningStatusEnabled)
		} else {
			vc.Status = aws.String(s3.BucketVersioningStatusSuspended)
		}

	} else {
		vc.Status = aws.String(s3.BucketVersioningStatusSuspended)
	}

	i := &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: vc,
	}
	log.Printf("[DEBUG] S3 put bucket versioning: %#v", i)

	_, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3conn.PutBucketVersioning(i)
	})
	if err != nil {
		return fmt.Errorf("Error putting S3 versioning: %s", err)
	}

	return nil
}

type yandexStorageTaggingHandleFunc func([]*s3.Tag) error

func resourceYandexStorageHandleTagsUpdate(
	d *schema.ResourceData,
	entityType string,
	onUpdate yandexStorageTaggingHandleFunc,
	onDelete func() error,
) error {
	tagsOldRaw, tagsNewRaw := d.GetChange("tags")

	var (
		needUpdate, needDelete bool
	)

	tagsOld := convertTypesMap(tagsOldRaw)
	tagsNew := convertTypesMap(tagsNewRaw)

	if len(tagsNew) == 0 {
		needDelete = true
	} else if len(tagsOld) != len(tagsNew) {
		needUpdate = true
	} else {
		for k, v := range tagsNew {
			oldv, ok := tagsOld[k]

			if !ok || v != oldv {
				log.Printf("[DEBUG] for key %s found new value: %s (old: %s)", k, oldv, v)

				needUpdate = true
				break
			}
		}
	}

	if !needUpdate && !needDelete {
		log.Printf("[DEBUG] Skipping Storage S3 %s tags update/delete since no changes were made", entityType)
		return nil
	}

	var err error
	switch {
	case needUpdate:
		tags := storageBucketTaggingFromMap(tagsNew)

		err = onUpdate(tags)
	case needDelete:
		err = onDelete()
	}

	return err
}

func resourceYandexStorageBucketTagsUpdate(s3conn *s3.S3, d *schema.ResourceData) error {
	bucket := aws.String(d.Get("bucket").(string))

	onUpdate := func(tags []*s3.Tag) error {
		log.Printf("[INFO] Updating Storage S3 bucket tags with %v", tags)

		request := &s3.PutBucketTaggingInput{
			Bucket: bucket,
			Tagging: &s3.Tagging{
				TagSet: tags,
			},
		}
		_, err := retryFlakyS3Responses(func() (interface{}, error) {
			return s3conn.PutBucketTagging(request)
		})
		if err != nil {
			log.Printf("[ERROR] Unable to update Storage S3 bucket tags: %s", err)
		}
		return err
	}

	onDelete := func() error {
		log.Printf("[INFO] Deleting Storage S3 bucket tags")

		request := &s3.DeleteBucketTaggingInput{
			Bucket: bucket,
		}
		_, err := retryFlakyS3Responses(func() (interface{}, error) {
			return s3conn.DeleteBucketTagging(request)
		})
		if err != nil {
			log.Printf("[ERROR] Unable to delete Storage S3 bucket tags: %s", err)
		}
		return err
	}

	return resourceYandexStorageHandleTagsUpdate(d, "bucket", onUpdate, onDelete)
}

func resourceYandexStorageBucketObjectLockConfigurationUpdate(s3conn *s3.S3, d *schema.ResourceData) error {
	ol := d.Get("object_lock_configuration").([]interface{})
	bucket := d.Get("bucket").(string)
	olc := &s3.ObjectLockConfiguration{}

	if len(ol) > 0 {
		config := ol[0].(map[string]interface{})

		enabled := config["object_lock_enabled"].(string)
		olc.ObjectLockEnabled = aws.String(enabled)

		rs := config["rule"].([]interface{})
		if len(rs) > 0 {
			r := &s3.ObjectLockRule{
				DefaultRetention: &s3.DefaultRetention{},
			}
			rule := rs[0].(map[string]interface{})
			drs := rule["default_retention"].([]interface{})
			retention := drs[0].(map[string]interface{})

			mode := retention["mode"].(string)
			r.DefaultRetention.Mode = aws.String(mode)

			if d, ok := retention["days"]; ok && d.(int) > 0 {
				days := int64(d.(int))
				r.DefaultRetention.Days = aws.Int64(days)
			}
			if y, ok := retention["years"]; ok && y.(int) > 0 {
				years := int64(y.(int))
				r.DefaultRetention.Years = aws.Int64(years)
			}

			olc.Rule = r
		}
	}

	i := &s3.PutObjectLockConfigurationInput{
		Bucket:                  aws.String(bucket),
		ObjectLockConfiguration: olc,
	}
	log.Printf("[DEBUG] S3 put object lock configuration: %#v", i)

	_, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3conn.PutObjectLockConfiguration(i)
	})
	if err != nil {
		return fmt.Errorf("Error putting S3 object lock configuration: %s", err)
	}

	return nil
}

func resourceYandexStorageBucketLoggingUpdate(s3conn *s3.S3, d *schema.ResourceData) error {
	logging := d.Get("logging").(*schema.Set).List()
	bucket := d.Get("bucket").(string)
	loggingStatus := &s3.BucketLoggingStatus{}

	if len(logging) > 0 {
		c := logging[0].(map[string]interface{})

		loggingEnabled := &s3.LoggingEnabled{}
		if val, ok := c["target_bucket"]; ok {
			loggingEnabled.TargetBucket = aws.String(val.(string))
		}
		if val, ok := c["target_prefix"]; ok {
			loggingEnabled.TargetPrefix = aws.String(val.(string))
		}

		loggingStatus.LoggingEnabled = loggingEnabled
	}

	i := &s3.PutBucketLoggingInput{
		Bucket:              aws.String(bucket),
		BucketLoggingStatus: loggingStatus,
	}
	log.Printf("[DEBUG] S3 put bucket logging: %#v", i)

	_, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3conn.PutBucketLogging(i)
	})
	if err != nil {
		return fmt.Errorf("Error putting S3 logging: %s", err)
	}

	return nil
}

func bucketDomainName(bucket string, endpointURL string) (string, error) {
	// Without a scheme the url will not be parsed as we expect
	// See https://github.com/golang/go/issues/19779
	if !strings.Contains(endpointURL, "//") {
		endpointURL = "//" + endpointURL
	}

	parse, err := url.Parse(endpointURL)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s.%s", bucket, parse.Hostname()), nil
}

type S3Website struct {
	Endpoint, Domain string
}

func retryOnAwsCodes(codes []string, f func() (interface{}, error)) (interface{}, error) {
	var resp interface{}
	err := resource.Retry(1*time.Minute, func() *resource.RetryError {
		var err error
		resp, err = f()
		if err != nil {
			awsErr, ok := err.(awserr.Error)
			if ok {
				for _, code := range codes {
					if awsErr.Code() == code {
						return resource.RetryableError(err)
					}
				}
			}
			return resource.NonRetryableError(err)
		}
		return nil
	})
	return resp, err
}

func retryFlakyS3Responses(f func() (interface{}, error)) (interface{}, error) {
	return retryOnAwsCodes([]string{"NoSuchBucket", "AccessDenied", "Forbidden"}, f)
}

func waitConditionStable(check func() (bool, error)) error {
	for checks := 0; checks < 12; checks++ {
		allOk := true
		for subchecks := 0; allOk && subchecks < 10; subchecks++ {
			ok, err := check()
			if err != nil {
				return err
			}
			allOk = allOk && ok
			if ok {
				time.Sleep(time.Second)
			}
		}
		if allOk {
			return nil
		}
		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("timeout exceeded")
}

func waitWebsitePut(s3Client *s3.S3, bucket string, configuration *s3.WebsiteConfiguration) error {
	input := &s3.GetBucketWebsiteInput{Bucket: aws.String(bucket)}

	check := func() (bool, error) {
		output, err := s3Client.GetBucketWebsite(input)
		if err != nil && !isAWSErr(err, "NoSuchWebsiteConfiguration", "") {
			return false, err
		}
		outputConfiguration := &s3.WebsiteConfiguration{
			ErrorDocument:         output.ErrorDocument,
			IndexDocument:         output.IndexDocument,
			RedirectAllRequestsTo: output.RedirectAllRequestsTo,
			RoutingRules:          output.RoutingRules,
		}
		if reflect.DeepEqual(outputConfiguration, configuration) {
			return true, nil
		}
		return false, nil
	}

	err := waitConditionStable(check)
	if err != nil {
		return fmt.Errorf("error assuring bucket %q website updated: %s", bucket, err)
	}
	return nil
}

func waitWebsiteDeleted(s3Client *s3.S3, bucket string) error {
	input := &s3.GetBucketWebsiteInput{Bucket: aws.String(bucket)}

	check := func() (bool, error) {
		_, err := s3Client.GetBucketWebsite(input)
		if isAWSErr(err, "NoSuchWebsiteConfiguration", "") {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	}

	err := waitConditionStable(check)
	if err != nil {
		return fmt.Errorf("error assuring bucket %q website deleted: %s", bucket, err)
	}
	return nil
}

func waitCorsPut(s3Client *s3.S3, bucket string, configuration *s3.CORSConfiguration) error {
	input := &s3.GetBucketCorsInput{Bucket: aws.String(bucket)}

	check := func() (bool, error) {
		output, err := s3Client.GetBucketCors(input)
		if err != nil && !isAWSErr(err, "NoSuchCORSConfiguration", "") {
			return false, err
		}
		empty := len(output.CORSRules) == 0 && len(configuration.CORSRules) == 0
		for _, rule := range output.CORSRules {
			if rule.ExposeHeaders == nil {
				rule.ExposeHeaders = make([]*string, 0)
			}
			if rule.AllowedHeaders == nil {
				rule.AllowedHeaders = make([]*string, 0)
			}
		}
		if empty || reflect.DeepEqual(output.CORSRules, configuration.CORSRules) {
			return true, nil
		}
		return false, nil
	}

	err := waitConditionStable(check)
	if err != nil {
		return fmt.Errorf("error assuring bucket %q CORS updated: %s", bucket, err)
	}
	return nil
}

func waitCorsDeleted(s3Client *s3.S3, bucket string) error {
	input := &s3.GetBucketCorsInput{Bucket: aws.String(bucket)}

	check := func() (bool, error) {
		_, err := s3Client.GetBucketCors(input)
		if isAWSErr(err, "NoSuchCORSConfiguration", "") {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	}

	err := waitConditionStable(check)
	if err != nil {
		return fmt.Errorf("error assuring bucket %q CORS deleted: %s", bucket, err)
	}
	return nil
}

// Returns true if the error matches all these conditions:
//   - err is of type awserr.Error
//   - Error.Code() matches code
//   - Error.Message() contains message
func isAWSErr(err error, code string, message string) bool {
	if err, ok := err.(awserr.Error); ok {
		return err.Code() == code && strings.Contains(err.Message(), message)
	}
	return false
}

func handleS3BucketNotFoundError(d *schema.ResourceData, err error) bool {
	if awsError, ok := err.(awserr.RequestFailure); ok && awsError.StatusCode() == 404 ||
		isStatusWithCode(err, codes.NotFound) {
		log.Printf("[WARN] Storage Bucket (%s) not found, error code (404)", d.Id())
		d.SetId("")
		return true
	}
	return false
}

// Takes list of pointers to strings. Expand to an array
// of raw strings and returns a []interface{}
// to keep compatibility w/ schema.NewSetschema.NewSet
func flattenStringList(list []*string) []interface{} {
	vs := make([]interface{}, 0, len(list))
	for _, v := range list {
		vs = append(vs, *v)
	}
	return vs
}

func validateS3BucketName(value string) error {
	if len(value) > 63 {
		return fmt.Errorf("%q must contain 63 characters at most", value)
	}
	if len(value) < 3 {
		return fmt.Errorf("%q must contain at least 3 characters", value)
	}
	if !regexp.MustCompile(`^[0-9a-zA-Z-.]+$`).MatchString(value) {
		return fmt.Errorf("only alphanumeric characters, hyphens, and periods allowed in %q", value)
	}

	return nil
}

func grantHash(v interface{}) int {
	var buf bytes.Buffer
	m, ok := v.(map[string]interface{})

	if !ok {
		return 0
	}

	if v, ok := m["id"]; ok {
		buf.WriteString(fmt.Sprintf("%s-", v.(string)))
	}
	if v, ok := m["type"]; ok {
		buf.WriteString(fmt.Sprintf("%s-", v.(string)))
	}
	if v, ok := m["uri"]; ok {
		buf.WriteString(fmt.Sprintf("%s-", v.(string)))
	}
	if p, ok := m["permissions"]; ok {
		buf.WriteString(fmt.Sprintf("%v-", p.(*schema.Set).List()))
	}
	return hashcode.String(buf.String())
}

func transitionHash(v interface{}) int {
	var buf bytes.Buffer
	m, ok := v.(map[string]interface{})

	if !ok {
		return 0
	}

	if v, ok := m["date"]; ok {
		buf.WriteString(fmt.Sprintf("%s-", v.(string)))
	}
	if v, ok := m["days"]; ok {
		buf.WriteString(fmt.Sprintf("%d-", v.(int)))
	}
	if v, ok := m["storage_class"]; ok {
		buf.WriteString(fmt.Sprintf("%s-", v.(string)))
	}
	return hashcode.String(buf.String())
}

func resourceYandexStorageBucketPolicyUpdate(s3conn *s3.S3, d *schema.ResourceData) error {
	bucket := d.Get("bucket").(string)
	policy := d.Get("policy").(string)

	if policy == "" {
		log.Printf("[DEBUG] S3 bucket: %s, delete policy: %s", bucket, policy)
		_, err := retryFlakyS3Responses(func() (interface{}, error) {
			return s3conn.DeleteBucketPolicy(&s3.DeleteBucketPolicyInput{
				Bucket: aws.String(bucket),
			})
		})

		if err != nil {
			return fmt.Errorf("Error deleting S3 policy: %s", err)
		}
		return nil
	}
	log.Printf("[DEBUG] S3 bucket: %s, put policy: %s", bucket, policy)

	params := &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(policy),
	}

	err := resource.Retry(1*time.Minute, func() *resource.RetryError {
		_, err := s3conn.PutBucketPolicy(params)
		if isAWSErr(err, "MalformedPolicy", "") || isAWSErr(err, s3.ErrCodeNoSuchBucket, "") {
			return resource.RetryableError(err)
		}
		if err != nil {
			return resource.NonRetryableError(err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("Error putting S3 policy: %s", err)
	}

	return nil
}

func resourceYandexStorageBucketGrantsUpdate(s3conn *s3.S3, d *schema.ResourceData) error {
	bucket := d.Get("bucket").(string)
	rawGrants := d.Get("grant").(*schema.Set).List()

	if len(rawGrants) == 0 {
		log.Printf("[DEBUG] Storage Bucket: %s, Grants fallback to canned ACL", bucket)
		if err := resourceYandexStorageBucketACLUpdate(s3conn, d); err != nil {
			return fmt.Errorf("error fallback to canned ACL, %s", err)
		}

		return nil
	}

	apResponse, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3conn.GetBucketAcl(&s3.GetBucketAclInput{
			Bucket: aws.String(bucket),
		})
	})

	if err != nil {
		return fmt.Errorf("error getting Storage Bucket (%s) ACL: %s", bucket, err)
	}

	ap := apResponse.(*s3.GetBucketAclOutput)
	log.Printf("[DEBUG] Storage Bucket: %s, read ACL grants policy: %+v", bucket, ap)

	grants := make([]*s3.Grant, 0, len(rawGrants))
	for _, rawGrant := range rawGrants {
		log.Printf("[DEBUG] Storage Bucket: %s, put grant: %#v", bucket, rawGrant)
		grantMap := rawGrant.(map[string]interface{})
		permissions := grantMap["permissions"].(*schema.Set).List()
		if err := validateBucketPermissions(permissions); err != nil {
			return err
		}
		for _, rawPermission := range permissions {
			ge := &s3.Grantee{}
			if i, ok := grantMap["id"].(string); ok && i != "" {
				ge.SetID(i)
			}
			if t, ok := grantMap["type"].(string); ok && t != "" {
				ge.SetType(t)
			}
			if u, ok := grantMap["uri"].(string); ok && u != "" {
				ge.SetURI(u)
			}

			g := &s3.Grant{
				Grantee:    ge,
				Permission: aws.String(rawPermission.(string)),
			}
			grants = append(grants, g)
		}
	}

	grantsInput := &s3.PutBucketAclInput{
		Bucket: aws.String(bucket),
		AccessControlPolicy: &s3.AccessControlPolicy{
			Grants: grants,
			Owner:  ap.Owner,
		},
	}

	log.Printf("[DEBUG] Bucket: %s, put Grants: %#v", bucket, grantsInput)

	_, err = retryFlakyS3Responses(func() (interface{}, error) {
		return s3conn.PutBucketAcl(grantsInput)
	})

	if err != nil {
		return fmt.Errorf("error putting Storage Bucket (%s) ACL: %s", bucket, err)
	}

	return nil
}

func resourceYandexStorageBucketLifecycleUpdate(s3conn *s3.S3, d *schema.ResourceData) error {
	bucket := d.Get("bucket").(string)

	lifecycleRules := d.Get("lifecycle_rule").([]interface{})

	if len(lifecycleRules) == 0 || lifecycleRules[0] == nil {
		i := &s3.DeleteBucketLifecycleInput{
			Bucket: aws.String(bucket),
		}

		_, err := s3conn.DeleteBucketLifecycle(i)
		if err != nil {
			return fmt.Errorf("Error removing S3 lifecycle: %s", err)
		}
		return nil
	}

	rules := make([]*s3.LifecycleRule, 0, len(lifecycleRules))

	for i, lifecycleRule := range lifecycleRules {
		r := lifecycleRule.(map[string]interface{})

		rule := &s3.LifecycleRule{}

		// Filter
		filter := &s3.LifecycleRuleFilter{}
		filter.SetPrefix(r["prefix"].(string))
		rule.SetFilter(filter)

		// ID
		if val, ok := r["id"].(string); ok && val != "" {
			rule.ID = aws.String(val)
		} else {
			rule.ID = aws.String(resource.PrefixedUniqueId("tf-s3-lifecycle-"))
		}

		// Enabled
		if val, ok := r["enabled"].(bool); ok && val {
			rule.Status = aws.String(s3.ExpirationStatusEnabled)
		} else {
			rule.Status = aws.String(s3.ExpirationStatusDisabled)
		}

		// AbortIncompleteMultipartUpload
		if val, ok := r["abort_incomplete_multipart_upload_days"].(int); ok && val > 0 {
			rule.AbortIncompleteMultipartUpload = &s3.AbortIncompleteMultipartUpload{
				DaysAfterInitiation: aws.Int64(int64(val)),
			}
		}

		// Expiration
		expiration := d.Get(fmt.Sprintf("lifecycle_rule.%d.expiration", i)).([]interface{})
		if len(expiration) > 0 && expiration[0] != nil {
			e := expiration[0].(map[string]interface{})
			i := &s3.LifecycleExpiration{}
			if val, ok := e["date"].(string); ok && val != "" {
				t, err := time.Parse(time.RFC3339, fmt.Sprintf("%sT00:00:00Z", val))
				if err != nil {
					return fmt.Errorf("Error Parsing AWS S3 Bucket Lifecycle Expiration Date: %s", err.Error())
				}
				i.Date = aws.Time(t)
			} else if val, ok := e["days"].(int); ok && val > 0 {
				i.Days = aws.Int64(int64(val))
			} else if val, ok := e["expired_object_delete_marker"].(bool); ok {
				i.ExpiredObjectDeleteMarker = aws.Bool(val)
			}
			rule.Expiration = i
		}

		// NoncurrentVersionExpiration
		nc_expiration := d.Get(fmt.Sprintf("lifecycle_rule.%d.noncurrent_version_expiration", i)).([]interface{})
		if len(nc_expiration) > 0 && nc_expiration[0] != nil {
			e := nc_expiration[0].(map[string]interface{})

			if val, ok := e["days"].(int); ok && val > 0 {
				rule.NoncurrentVersionExpiration = &s3.NoncurrentVersionExpiration{
					NoncurrentDays: aws.Int64(int64(val)),
				}
			}
		}

		// Transitions
		transitions := d.Get(fmt.Sprintf("lifecycle_rule.%d.transition", i)).(*schema.Set).List()
		if len(transitions) > 0 {
			rule.Transitions = make([]*s3.Transition, 0, len(transitions))
			for _, transition := range transitions {
				transition := transition.(map[string]interface{})
				i := &s3.Transition{}
				if val, ok := transition["date"].(string); ok && val != "" {
					t, err := time.Parse(time.RFC3339, fmt.Sprintf("%sT00:00:00Z", val))
					if err != nil {
						return fmt.Errorf("Error Parsing AWS S3 Bucket Lifecycle Expiration Date: %s", err.Error())
					}
					i.Date = aws.Time(t)
				} else if val, ok := transition["days"].(int); ok && val >= 0 {
					i.Days = aws.Int64(int64(val))
				}
				if val, ok := transition["storage_class"].(string); ok && val != "" {
					i.StorageClass = aws.String(val)
				}

				rule.Transitions = append(rule.Transitions, i)
			}
		}
		// NoncurrentVersionTransitions
		nc_transitions := d.Get(fmt.Sprintf("lifecycle_rule.%d.noncurrent_version_transition", i)).(*schema.Set).List()
		if len(nc_transitions) > 0 {
			rule.NoncurrentVersionTransitions = make([]*s3.NoncurrentVersionTransition, 0, len(nc_transitions))
			for _, transition := range nc_transitions {
				transition := transition.(map[string]interface{})
				i := &s3.NoncurrentVersionTransition{}
				if val, ok := transition["days"].(int); ok && val >= 0 {
					i.NoncurrentDays = aws.Int64(int64(val))
				}
				if val, ok := transition["storage_class"].(string); ok && val != "" {
					i.StorageClass = aws.String(val)
				}

				rule.NoncurrentVersionTransitions = append(rule.NoncurrentVersionTransitions, i)
			}
		}

		// As a lifecycle rule requires 1 or more transition/expiration actions,
		// we explicitly pass a default ExpiredObjectDeleteMarker value to be able to create
		// the rule while keeping the policy unaffected if the conditions are not met.
		if rule.Expiration == nil && rule.NoncurrentVersionExpiration == nil &&
			rule.Transitions == nil && rule.NoncurrentVersionTransitions == nil &&
			rule.AbortIncompleteMultipartUpload == nil {
			rule.Expiration = &s3.LifecycleExpiration{ExpiredObjectDeleteMarker: aws.Bool(false)}
		}

		rules = append(rules, rule)
	}

	i := &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucket),
		LifecycleConfiguration: &s3.BucketLifecycleConfiguration{
			Rules: rules,
		},
	}

	_, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3conn.PutBucketLifecycleConfiguration(i)
	})
	if err != nil {
		return fmt.Errorf("Error putting S3 lifecycle: %s", err)
	}

	return nil
}

func resourceYandexStorageBucketServerSideEncryptionConfigurationUpdate(s3conn *s3.S3, d *schema.ResourceData) error {
	bucket := d.Get("bucket").(string)
	serverSideEncryptionConfiguration := d.Get("server_side_encryption_configuration").([]interface{})
	if len(serverSideEncryptionConfiguration) == 0 {
		log.Printf("[DEBUG] Delete server side encryption configuration: %#v", serverSideEncryptionConfiguration)
		i := &s3.DeleteBucketEncryptionInput{
			Bucket: aws.String(bucket),
		}

		_, err := s3conn.DeleteBucketEncryption(i)
		if err != nil {
			return fmt.Errorf("error removing S3 bucket server side encryption: %s", err)
		}
		return nil
	}

	c := serverSideEncryptionConfiguration[0].(map[string]interface{})

	rc := &s3.ServerSideEncryptionConfiguration{}

	rcRules := c["rule"].([]interface{})
	var rules []*s3.ServerSideEncryptionRule
	for _, v := range rcRules {
		rr := v.(map[string]interface{})
		rrDefault := rr["apply_server_side_encryption_by_default"].([]interface{})
		sseAlgorithm := rrDefault[0].(map[string]interface{})["sse_algorithm"].(string)
		kmsMasterKeyId := rrDefault[0].(map[string]interface{})["kms_master_key_id"].(string)
		rcDefaultRule := &s3.ServerSideEncryptionByDefault{
			SSEAlgorithm: aws.String(sseAlgorithm),
		}
		if kmsMasterKeyId != "" {
			rcDefaultRule.KMSMasterKeyID = aws.String(kmsMasterKeyId)
		}
		rcRule := &s3.ServerSideEncryptionRule{
			ApplyServerSideEncryptionByDefault: rcDefaultRule,
		}

		rules = append(rules, rcRule)
	}

	rc.Rules = rules
	i := &s3.PutBucketEncryptionInput{
		Bucket:                            aws.String(bucket),
		ServerSideEncryptionConfiguration: rc,
	}
	log.Printf("[DEBUG] S3 put bucket replication configuration: %#v", i)

	_, err := retryFlakyS3Responses(func() (interface{}, error) {
		return s3conn.PutBucketEncryption(i)
	})
	if err != nil {
		return fmt.Errorf("error putting S3 server side encryption configuration: %s", err)
	}

	return nil
}

func flattenGrants(ap *s3.GetBucketAclOutput) []interface{} {
	//if ACL grants contains bucket owner FULL_CONTROL only - it is default "private" acl
	if len(ap.Grants) == 1 && aws.StringValue(ap.Grants[0].Grantee.ID) == aws.StringValue(ap.Owner.ID) &&
		aws.StringValue(ap.Grants[0].Permission) == s3.PermissionFullControl {
		return nil
	}

	getGrant := func(grants []interface{}, grantee map[string]interface{}) (interface{}, bool) {
		for _, pg := range grants {
			pgt := pg.(map[string]interface{})
			if pgt["type"] == grantee["type"] && pgt["id"] == grantee["id"] && pgt["uri"] == grantee["uri"] &&
				pgt["permissions"].(*schema.Set).Len() > 0 {
				return pg, true
			}
		}
		return nil, false
	}

	grants := make([]interface{}, 0, len(ap.Grants))
	for _, granteeObject := range ap.Grants {
		grantee := make(map[string]interface{})
		grantee["type"] = aws.StringValue(granteeObject.Grantee.Type)

		if granteeObject.Grantee.ID != nil {
			grantee["id"] = aws.StringValue(granteeObject.Grantee.ID)
		}
		if granteeObject.Grantee.URI != nil {
			grantee["uri"] = aws.StringValue(granteeObject.Grantee.URI)
		}
		if pg, ok := getGrant(grants, grantee); ok {
			pg.(map[string]interface{})["permissions"].(*schema.Set).Add(aws.StringValue(granteeObject.Permission))
		} else {
			grantee["permissions"] = schema.NewSet(schema.HashString, []interface{}{aws.StringValue(granteeObject.Permission)})
			grants = append(grants, grantee)
		}
	}

	return grants
}

func flattenS3ServerSideEncryptionConfiguration(c *s3.ServerSideEncryptionConfiguration) []map[string]interface{} {
	var encryptionConfiguration []map[string]interface{}
	rules := make([]interface{}, 0, len(c.Rules))
	for _, v := range c.Rules {
		if v.ApplyServerSideEncryptionByDefault != nil {
			r := make(map[string]interface{})
			d := make(map[string]interface{})
			d["kms_master_key_id"] = aws.StringValue(v.ApplyServerSideEncryptionByDefault.KMSMasterKeyID)
			d["sse_algorithm"] = aws.StringValue(v.ApplyServerSideEncryptionByDefault.SSEAlgorithm)
			r["apply_server_side_encryption_by_default"] = []map[string]interface{}{d}
			rules = append(rules, r)
		}
	}
	encryptionConfiguration = append(encryptionConfiguration, map[string]interface{}{
		"rule": rules,
	})
	return encryptionConfiguration
}

func validateBucketPermissions(permissions []interface{}) error {
	var (
		fullControl     bool
		permissionRead  bool
		permissionWrite bool
	)

	for _, p := range permissions {
		s := p.(string)
		switch s {
		case s3.PermissionFullControl:
			fullControl = true
		case s3.PermissionRead:
			permissionRead = true
		case s3.PermissionWrite:
			permissionWrite = true
		}
	}

	if fullControl && len(permissions) > 1 {
		return fmt.Errorf("do not use other ACP permissions along with `FULL_CONTROL` permission for Storage Bucket")
	}

	if permissionWrite && !permissionRead {
		return fmt.Errorf("should always provide `READ` permission, when granting `WRITE` for Storage Bucket")
	}

	return nil
}

func validateStringIsJSON(i interface{}, k string) (warnings []string, errors []error) {
	v, ok := i.(string)
	if !ok {
		errors = append(errors, fmt.Errorf("expected type of %s to be string", k))
		return warnings, errors
	}

	if _, err := NormalizeJsonString(v); err != nil {
		errors = append(errors, fmt.Errorf("%q contains an invalid JSON: %s", k, err))
	}

	return warnings, errors
}

func NormalizeJsonString(jsonString interface{}) (string, error) {
	var j interface{}

	if jsonString == nil || jsonString.(string) == "" {
		return "", nil
	}

	s := jsonString.(string)

	err := json.Unmarshal([]byte(s), &j)
	if err != nil {
		return "", err
	}

	bytes, _ := json.Marshal(j)
	return string(bytes[:]), nil
}

func normalizeRoutingRules(w []*s3.RoutingRule) (string, error) {
	withNulls, err := json.Marshal(w)
	if err != nil {
		return "", err
	}

	var rules []map[string]interface{}
	if err := json.Unmarshal(withNulls, &rules); err != nil {
		return "", err
	}

	var cleanRules []map[string]interface{}
	for _, rule := range rules {
		cleanRules = append(cleanRules, removeNil(rule))
	}

	withoutNulls, err := json.Marshal(cleanRules)
	if err != nil {
		return "", err
	}

	return string(withoutNulls), nil
}

func removeNil(data map[string]interface{}) map[string]interface{} {
	withoutNil := make(map[string]interface{})

	for k, v := range data {
		if v == nil {
			continue
		}

		switch v := v.(type) {
		case map[string]interface{}:
			withoutNil[k] = removeNil(v)
		default:
			withoutNil[k] = v
		}
	}

	return withoutNil
}

func suppressEquivalentAwsPolicyDiffs(_, old, new string, _ *schema.ResourceData) bool {
	equivalent, err := awspolicy.PoliciesAreEquivalent(old, new)
	if err != nil {
		return false
	}

	return equivalent
}

func storageBucketS3SetFunc(keys ...string) schema.SchemaSetFunc {
	return func(v interface{}) int {
		var buf bytes.Buffer
		m, ok := v.(map[string]interface{})

		if !ok {
			return 0
		}

		for _, key := range keys {
			if v, ok := m[key]; ok {
				value := fmt.Sprintf("%v", v)
				buf.WriteString(value + "-")
			}
		}

		return hashcode.String(buf.String())
	}
}

func getAnonymousAccessFlagsSDK(value interface{}) *storagepb.AnonymousAccessFlags {
	schemaSet, ok := value.(*schema.Set)
	if !ok || schemaSet.Len() == 0 {
		return nil
	}

	accessFlags := new(storagepb.AnonymousAccessFlags)
	flags := schemaSet.List()[0].(map[string]interface{})
	if val, ok := flags["list"].(bool); ok {
		accessFlags.List = wrapperspb.Bool(val)
	}
	if val, ok := flags["read"].(bool); ok {
		accessFlags.Read = wrapperspb.Bool(val)
	}
	if val, ok := flags["config_read"].(bool); ok {
		accessFlags.ConfigRead = wrapperspb.Bool(val)
	}

	return accessFlags
}

func storageBucketTaggingNormalize(tags []*s3.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}

	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		out[*tag.Key] = *tag.Value
	}

	return out
}

func storageBucketTaggingFromMap(tags map[string]string) []*s3.Tag {
	out := make([]*s3.Tag, 0, len(tags))
	for k, v := range tags {
		out = append(out, &s3.Tag{
			Key:   aws.String(k),
			Value: aws.String(v),
		})
	}

	return out
}

func convertTypesMap(in interface{}) map[string]string {
	if in == nil {
		return nil
	}

	typedValue, ok := in.(map[string]interface{})
	if !ok {
		return nil
	}

	out := make(map[string]string, len(typedValue))

	for k, v := range typedValue {
		value, ok := v.(string)
		if !ok {
			continue
		}

		out[k] = value
	}

	return out
}
