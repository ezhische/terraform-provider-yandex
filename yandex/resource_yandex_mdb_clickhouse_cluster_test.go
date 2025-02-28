package yandex

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/genproto/protobuf/field_mask"

	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"

	"github.com/yandex-cloud/go-genproto/yandex/cloud/mdb/clickhouse/v1"
	cfg "github.com/yandex-cloud/go-genproto/yandex/cloud/mdb/clickhouse/v1/config"
)

const chVersion = "22.8"
const chUpdatedVersion = "23.3"
const chResource = "yandex_mdb_clickhouse_cluster.foo"
const chResourceSharded = "yandex_mdb_clickhouse_cluster.bar"
const chResourceCloudStorage = "yandex_mdb_clickhouse_cluster.cloud"
const chResourceKeeper = "yandex_mdb_clickhouse_cluster.keeper"

const (
	MaintenanceWindowAnytime = "type = \"ANYTIME\""
	MaintenanceWindowWeekly  = "type = \"WEEKLY\"\n    day  = \"FRI\"\n    hour = 20"
)

var StorageEndpointUrl = getStorageEndpointUrl()

func getStorageEndpointUrl() string {
	rawUrl := os.Getenv("YC_STORAGE_ENDPOINT_URL")
	const protocol = "https://"
	if strings.HasPrefix(rawUrl, protocol) {
		return rawUrl
	}
	return fmt.Sprintf("%s%s", protocol, rawUrl)
}

func init() {
	resource.AddTestSweepers("yandex_mdb_clickhouse_cluster", &resource.Sweeper{
		Name: "yandex_mdb_clickhouse_cluster",
		F:    testSweepMDBClickHouseCluster,
	})
}

func testSweepMDBClickHouseCluster(_ string) error {
	conf, err := configForSweepers()
	if err != nil {
		return fmt.Errorf("error getting client: %s", err)
	}

	resp, err := conf.sdk.MDB().Clickhouse().Cluster().List(conf.Context(), &clickhouse.ListClustersRequest{
		FolderId: conf.FolderID,
		PageSize: defaultMDBPageSize,
	})
	if err != nil {
		return fmt.Errorf("error getting ClickHouse clusters: %s", err)
	}

	result := &multierror.Error{}
	for _, c := range resp.Clusters {
		if !sweepMDBClickHouseCluster(conf, c.Id) {
			result = multierror.Append(result, fmt.Errorf("failed to sweep ClickHouse cluster %q", c.Id))
		}
	}

	return result.ErrorOrNil()
}

func sweepMDBClickHouseCluster(conf *Config, id string) bool {
	return sweepWithRetry(sweepMDBClickHouseClusterOnce, conf, "ClickHouse cluster", id)
}

func sweepMDBClickHouseClusterOnce(conf *Config, id string) error {
	ctx, cancel := conf.ContextWithTimeout(yandexMDBClickHouseClusterDeleteTimeout)
	defer cancel()

	mask := field_mask.FieldMask{Paths: []string{"deletion_protection"}}
	op, err := conf.sdk.MDB().Clickhouse().Cluster().Update(ctx, &clickhouse.UpdateClusterRequest{
		ClusterId:          id,
		DeletionProtection: false,
		UpdateMask:         &mask,
	})
	err = handleSweepOperation(ctx, conf, op, err)
	if err != nil && !strings.EqualFold(errorMessage(err), "no changes detected") {
		return err
	}

	op, err = conf.sdk.MDB().Clickhouse().Cluster().Delete(ctx, &clickhouse.DeleteClusterRequest{
		ClusterId: id,
	})
	return handleSweepOperation(ctx, conf, op, err)
}

func mdbClickHouseClusterImportStep(name string) resource.TestStep {
	return resource.TestStep{
		ResourceName:      name,
		ImportState:       true,
		ImportStateVerify: true,
		ImportStateVerifyIgnore: []string{
			"user",                              // passwords are not returned
			"host",                              // zookeeper hosts are not imported by default
			"zookeeper",                         // zookeeper spec is not imported by default
			"health",                            // volatile value
			"copy_schema_on_new_hosts",          // special parameter
			"admin_password",                    // passwords are not returned
			"clickhouse.0.config.0.kafka",       // passwords are not returned
			"clickhouse.0.config.0.kafka_topic", // passwords are not returned
			"clickhouse.0.config.0.rabbitmq",    // passwords are not returned
		},
	}
}

// Test that a ClickHouse Cluster can be created, updated and destroyed
func TestAccMDBClickHouseCluster_full(t *testing.T) {
	t.Parallel()

	var r clickhouse.Cluster
	chName := acctest.RandomWithPrefix("tf-clickhouse")
	folderID := getExampleFolderID()
	bucketName := acctest.RandomWithPrefix("tf-test-clickhouse-bucket")
	rInt := acctest.RandInt()

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckMDBClickHouseClusterDestroy,
		Steps: []resource.TestStep{
			// Create ClickHouse Cluster with anytime maintenance_window
			{
				Config: testAccMDBClickHouseClusterConfigMain(chName, "Step 1", "PRESTABLE", false, bucketName, rInt, MaintenanceWindowAnytime),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),

					resource.TestCheckResourceAttr(chResource, "security_group_ids.#", "1"),
					resource.TestCheckResourceAttrSet(chResource, "service_account_id"),
					resource.TestCheckResourceAttrSet(chResource, "host.0.fqdn"),

					resource.TestCheckResourceAttr(chResource, "access.0.web_sql", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.data_lens", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.metrika", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.serverless", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.data_transfer", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.yandex_query", "true"),

					testAccCheckMDBClickHouseClusterContainsLabel(&r, "test_key", "test_value"),
					testAccCheckMDBClickHouseClusterHasUsers(chResource, map[string][]string{"john": {"testdb"}},
						map[string]map[string]interface{}{
							"john": {
								"add_http_cors_header":          true,
								"connect_timeout":               42000,
								"count_distinct_implementation": "uniq_combined_64"}},
						map[string][]map[string]interface{}{},
					),
					testAccCheckMDBClickHouseClusterHasDatabases(chResource, []string{"testdb"}),
					testAccCheckMDBClickHouseClusterHasFormatSchemas(chResource, map[string]map[string]string{}),
					testAccCheckMDBClickHouseClusterHasMlModels(chResource, map[string]map[string]string{}),
					testAccCheckCreatedAtAttr(chResource),
					resource.TestCheckResourceAttr(chResource, "maintenance_window.0.type", "ANYTIME"),
					resource.TestCheckResourceAttr(chResource, "deletion_protection", "false"),
				),
			},
			mdbClickHouseClusterImportStep(chResource),
			// Update ClickHouse Cluster with weekly maintenance_window
			{
				Config: testAccMDBClickHouseClusterConfigMain(chName, "Step 2", "PRESTABLE", true, bucketName, rInt, MaintenanceWindowWeekly),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),

					resource.TestCheckResourceAttr(chResource, "security_group_ids.#", "1"),
					resource.TestCheckResourceAttrSet(chResource, "service_account_id"),
					resource.TestCheckResourceAttrSet(chResource, "host.0.fqdn"),

					resource.TestCheckResourceAttr(chResource, "access.0.web_sql", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.data_lens", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.metrika", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.serverless", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.data_transfer", "true"),
					resource.TestCheckResourceAttr(chResource, "access.0.yandex_query", "true"),

					testAccCheckMDBClickHouseClusterContainsLabel(&r, "test_key", "test_value"),
					testAccCheckMDBClickHouseClusterHasUsers(chResource, map[string][]string{"john": {"testdb"}},
						map[string]map[string]interface{}{
							"john": {
								"add_http_cors_header":          true,
								"connect_timeout":               42000,
								"count_distinct_implementation": "uniq_combined_64"}},
						map[string][]map[string]interface{}{},
					),
					testAccCheckMDBClickHouseClusterHasDatabases(chResource, []string{"testdb"}),
					testAccCheckMDBClickHouseClusterHasFormatSchemas(chResource, map[string]map[string]string{}),
					testAccCheckMDBClickHouseClusterHasMlModels(chResource, map[string]map[string]string{}),
					testAccCheckCreatedAtAttr(chResource),
					resource.TestCheckResourceAttr(chResource, "maintenance_window.0.type", "WEEKLY"),
					resource.TestCheckResourceAttr(chResource, "maintenance_window.0.day", "FRI"),
					resource.TestCheckResourceAttr(chResource, "maintenance_window.0.hour", "20"),
					resource.TestCheckResourceAttr(chResource, "deletion_protection", "true"),
				),
			},
			mdbClickHouseClusterImportStep(chResource),
			// test 'deletion_protection
			{
				Config:      testAccMDBClickHouseClusterConfigMain(chName, "Step 3", "PRODUCTION", true, bucketName, rInt, MaintenanceWindowWeekly),
				ExpectError: regexp.MustCompile(".*The operation was rejected because cluster has 'deletion_protection' = ON.*"),
			},
			mdbClickHouseClusterImportStep(chResource),
			// Change some options
			{
				Config: testAccMDBClickHouseClusterConfigUpdated(chName, "Step 4", bucketName, rInt),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),

					resource.TestCheckResourceAttr(chResource, "security_group_ids.#", "2"),
					resource.TestCheckResourceAttrSet(chResource, "host.0.fqdn"),
					testAccCheckMDBClickHouseClusterContainsLabel(&r, "new_key", "new_value"),
					testAccCheckMDBClickHouseClusterHasUsers(chResource, map[string][]string{"john": {"testdb"}, "mary": {"newdb", "testdb"}},
						map[string]map[string]interface{}{
							"john": {
								"add_http_cors_header":          true,
								"connect_timeout":               44000,
								"count_distinct_implementation": "uniq_combined_64"}},
						map[string][]map[string]interface{}{
							"mary": {
								{"interval_duration": 3600000, "queries": 1000},
								{"interval_duration": 79800000, "queries": 5000},
							},
						},
					),
					testAccCheckMDBClickHouseClusterHasDatabases(chResource, []string{"testdb", "newdb"}),
					testAccCheckMDBClickHouseClusterHasFormatSchemas(chResource, map[string]map[string]string{
						"test_schema": {
							"type": "FORMAT_SCHEMA_TYPE_CAPNPROTO",
							"uri":  fmt.Sprintf("%s/%s/test.capnp", StorageEndpointUrl, bucketName),
						},
					}),
					testAccCheckMDBClickHouseClusterHasMlModels(chResource, map[string]map[string]string{
						"test_model": {
							"type": "ML_MODEL_TYPE_CATBOOST",
							"uri":  fmt.Sprintf("%s/%s/train.csv", StorageEndpointUrl, bucketName),
						},
					}),
					testAccCheckCreatedAtAttr(chResource),
					resource.TestCheckResourceAttr(chResource, "maintenance_window.0.type", "ANYTIME"),
					resource.TestCheckResourceAttr(chResource, "cloud_storage.0.enabled", "true"),
					resource.TestCheckResourceAttr(chResource, "deletion_protection", "false"),
				),
			},
			mdbClickHouseClusterImportStep(chResource),
			// Check quota, schemas, model, users
			{
				Config: testAccMDBClickHouseClusterConfigUser(chName, "Step 5", bucketName, rInt),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),

					resource.TestCheckResourceAttr(chResource, "security_group_ids.#", "1"),
					resource.TestCheckResourceAttrSet(chResource, "host.0.fqdn"),
					testAccCheckMDBClickHouseClusterContainsLabel(&r, "new_key", "new_value"),
					testAccCheckMDBClickHouseClusterHasUsers(chResource, map[string][]string{"john": {"testdb"}, "mary": {"newdb", "testdb"}},
						map[string]map[string]interface{}{
							"john": {
								"add_http_cors_header":          true,
								"connect_timeout":               44000,
								"count_distinct_implementation": "uniq_hll_12"}},
						map[string][]map[string]interface{}{
							"mary": {
								{"interval_duration": 3600000, "queries": 2000},
								{"interval_duration": 7200000, "queries": 3000},
								{"interval_duration": 79800000, "queries": 5000},
							},
						},
					),
					testAccCheckMDBClickHouseClusterHasDatabases(chResource, []string{"testdb", "newdb"}),
					testAccCheckCreatedAtAttr(chResource),
					testAccCheckMDBClickHouseClusterHasFormatSchemas(chResource, map[string]map[string]string{
						"test_schema": {
							"type": "FORMAT_SCHEMA_TYPE_CAPNPROTO",
							"uri":  fmt.Sprintf("%s/%s/test2.capnp", StorageEndpointUrl, bucketName),
						},
						"test_schema2": {
							"type": "FORMAT_SCHEMA_TYPE_PROTOBUF",
							"uri":  fmt.Sprintf("%s/%s/test.proto", StorageEndpointUrl, bucketName),
						},
					}),
					testAccCheckMDBClickHouseClusterHasMlModels(chResource, map[string]map[string]string{
						"test_model": {
							"type": "ML_MODEL_TYPE_CATBOOST",
							"uri":  fmt.Sprintf("%s/%s/train.csv", StorageEndpointUrl, bucketName),
						},
						"test_model2": {
							"type": "ML_MODEL_TYPE_CATBOOST",
							"uri":  fmt.Sprintf("%s/%s/train.csv", StorageEndpointUrl, bucketName),
						},
					}),
				),
			},
			mdbClickHouseClusterImportStep(chResource),
			// Enable sql_user_management and sql_database_management - requires replacement
			{
				Config: testAccMDBClickHouseClusterConfigSqlManaged(chName, "Step 6", bucketName, rInt),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),

					resource.TestCheckResourceAttrSet(chResource, "host.0.fqdn"),
					testAccCheckMDBClickHouseClusterContainsLabel(&r, "test_key", "test_value"),
					testAccCheckMDBClickHouseClusterHasUsers(chResource, map[string][]string{}, map[string]map[string]interface{}{}, map[string][]map[string]interface{}{}),
					testAccCheckMDBClickHouseClusterHasDatabases(chResource, []string{}),
					testAccCheckCreatedAtAttr(chResource)),
			},
			mdbClickHouseClusterImportStep(chResource),
		},
	})
}

// Test that a Keeper-based ClickHouse Cluster can be created and destroyed
func TestAccMDBClickHouseCluster_keeper(t *testing.T) {
	t.Parallel()

	var r clickhouse.Cluster
	chName := acctest.RandomWithPrefix("tf-clickhouse-keeper")
	chDesc := "ClickHouse Cluster Keeper Test"
	folderID := getExampleFolderID()
	bucketName := acctest.RandomWithPrefix("tf-test-clickhouse-bucket")
	rInt := acctest.RandInt()

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckMDBClickHouseClusterDestroy,
		Steps: []resource.TestStep{
			// Enable embedded_keeper
			{
				Config: testAccMDBClickHouseClusterConfigEmbeddedKeeper(chName, chDesc, bucketName, rInt),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResourceKeeper, &r, 1),
					resource.TestCheckResourceAttr(chResourceKeeper, "name", chName),
					resource.TestCheckResourceAttr(chResourceKeeper, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResourceKeeper, "description", chDesc),
					resource.TestCheckResourceAttrSet(chResourceKeeper, "host.0.fqdn"),
					testAccCheckMDBClickHouseClusterContainsLabel(&r, "test_key", "test_value"),
					testAccCheckMDBClickHouseClusterHasResources(&r, "s2.micro", "network-ssd", 17179869184),
					testAccCheckMDBClickHouseClusterHasUsers(chResourceKeeper, map[string][]string{}, map[string]map[string]interface{}{}, map[string][]map[string]interface{}{}),
					testAccCheckMDBClickHouseClusterHasDatabases(chResourceKeeper, []string{}),
					testAccCheckCreatedAtAttr(chResourceKeeper)),
			},
			mdbClickHouseClusterImportStep(chResourceKeeper),
		},
	})
}

/**
* Test that a sharded ClickHouse Cluster can be created, updated and destroyed.
* Also it checks changes shard's configuration.
 */
func TestAccMDBClickHouseCluster_sharded(t *testing.T) {
	t.Parallel()

	var r clickhouse.Cluster
	chName := acctest.RandomWithPrefix("tf-clickhouse-sharded")
	folderID := getExampleFolderID()
	bucketName := acctest.RandomWithPrefix("tf-test-clickhouse-bucket")
	rInt := acctest.RandInt()

	const createClusterDiskSize = 10
	const createFirstShardDiskSize = 11
	const createSecondShardDiskSize = 12

	const updateClusterDiskSize = 15

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckMDBClickHouseClusterDestroy,
		Steps: []resource.TestStep{
			// Create sharded ClickHouse Cluster
			{
				Config: testAccMDBClickHouseClusterConfigSharded(chName, createClusterDiskSize, createFirstShardDiskSize, createSecondShardDiskSize, bucketName, rInt),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResourceSharded, &r, 2),
					resource.TestCheckResourceAttr(chResourceSharded, "name", chName),
					resource.TestCheckResourceAttr(chResourceSharded, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.0.name", "shard1"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.0.weight", "11"),

					resource.TestCheckResourceAttr(chResourceSharded, "shard.0.resources.0.disk_size", strconv.Itoa(createFirstShardDiskSize)),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.0.resources.0.resource_preset_id", "s3-c4-m16"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.0.resources.0.disk_type_id", "network-ssd"),

					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.name", "shard2"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.weight", "22"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.resources.0.disk_size", strconv.Itoa(createSecondShardDiskSize)),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.resources.0.resource_preset_id", "s3-c2-m8"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.resources.0.disk_type_id", "network-ssd"),

					resource.TestCheckResourceAttrSet(chResourceSharded, "host.0.fqdn"),
					testAccCheckMDBClickHouseClusterHasShards(&r, []string{"shard1", "shard2"}),
					testAccCheckMDBClickHouseClusterHasShardGroups(&r, map[string][]string{
						"test_group":   {"shard1", "shard2"},
						"test_group_2": {"shard1"},
					}),
					testAccCheckMDBClickHouseClusterHasUsers(chResourceSharded, map[string][]string{"john": {"testdb"}}, map[string]map[string]interface{}{}, map[string][]map[string]interface{}{}),
					testAccCheckMDBClickHouseClusterHasDatabases(chResourceSharded, []string{"testdb"}),
					testAccCheckCreatedAtAttr(chResourceSharded),
				),
			},
			mdbClickHouseClusterImportStep(chResourceSharded),
			// Add new shard, delete old shard
			{
				Config: testAccMDBClickHouseClusterConfigShardedUpdated(chName, updateClusterDiskSize, bucketName, rInt),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResourceSharded, &r, 2),
					resource.TestCheckResourceAttr(chResourceSharded, "name", chName),
					resource.TestCheckResourceAttr(chResourceSharded, "folder_id", folderID),

					resource.TestCheckResourceAttr(chResourceSharded, "shard.0.name", "shard1"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.0.weight", "110"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.0.resources.0.disk_size", strconv.Itoa(updateClusterDiskSize)),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.0.resources.0.resource_preset_id", "s3-c2-m8"),

					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.name", "shard3"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.weight", "330"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.resources.0.disk_size", strconv.Itoa(updateClusterDiskSize)),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.resources.0.resource_preset_id", "s3-c2-m8"),
					resource.TestCheckResourceAttr(chResourceSharded, "shard.1.resources.0.disk_type_id", "network-ssd"),

					resource.TestCheckResourceAttrSet(chResourceSharded, "host.0.fqdn"),
					testAccCheckMDBClickHouseClusterHasShards(&r, []string{"shard1", "shard3"}),
					testAccCheckMDBClickHouseClusterHasShardGroups(&r, map[string][]string{
						"test_group":   {"shard1", "shard3"},
						"test_group_3": {"shard1"},
					}),
					testAccCheckMDBClickHouseClusterHasResources(&r, "s3-c2-m8", "network-ssd", toBytes(updateClusterDiskSize)),
					testAccCheckMDBClickHouseClusterHasUsers(chResourceSharded, map[string][]string{"john": {"testdb"}}, map[string]map[string]interface{}{}, map[string][]map[string]interface{}{}),
					testAccCheckMDBClickHouseClusterHasDatabases(chResourceSharded, []string{"testdb"}),
					testAccCheckCreatedAtAttr(chResourceSharded),
				),
			},
			mdbClickHouseClusterImportStep(chResourceSharded),
		},
	})
}

// Test that a ClickHouse Cluster with cloud storage can be created
func TestAccMDBClickHouseCluster_cloud_storage(t *testing.T) {
	t.Parallel()

	var r clickhouse.Cluster
	chName := acctest.RandomWithPrefix("tf-clickhouse-cloud-storage")
	chDesc := "ClickHouse Cloud Storage Cluster Terraform Test"
	folderID := getExampleFolderID()
	bucketName := acctest.RandomWithPrefix("tf-test-clickhouse-bucket")
	rInt := acctest.RandInt()

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckMDBClickHouseClusterDestroy,
		Steps: []resource.TestStep{
			// Create ClickHouse Cluster with cloud storage
			{
				Config: testAccMDBClickHouseClusterConfigDefaultCloudStorage(chName, chDesc, bucketName, rInt),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResourceCloudStorage, &r, 1),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "name", chName),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "description", chDesc),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "cloud_storage.0.enabled", "false"),
					testAccCheckCreatedAtAttr(chResourceCloudStorage)),
			},
			mdbClickHouseClusterImportStep(chResourceCloudStorage),
			// Update ClickHouse Cluster with cloud storage
			{
				Config: testAccMDBClickHouseClusterConfigCloudStorage(chName, chDesc, bucketName, rInt, false, 0.0, false, 0),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResourceCloudStorage, &r, 1),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "name", chName),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "description", chDesc),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "cloud_storage.0.enabled", "false"),
					testAccCheckCreatedAtAttr(chResourceCloudStorage)),
			},
			mdbClickHouseClusterImportStep(chResourceCloudStorage),
			// Update ClickHouse Cluster with cloud storage with all params
			{
				Config: testAccMDBClickHouseClusterConfigCloudStorage(chName, chDesc, bucketName, rInt, true, 0.5, true, 214748364),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResourceCloudStorage, &r, 1),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "name", chName),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "description", chDesc),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "cloud_storage.0.enabled", "true"),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "cloud_storage.0.move_factor", "0.5"),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "cloud_storage.0.data_cache_enabled", "true"),
					resource.TestCheckResourceAttr(chResourceCloudStorage, "cloud_storage.0.data_cache_max_size", "214748364"),
					testAccCheckCreatedAtAttr(chResourceCloudStorage)),
			},
			mdbClickHouseClusterImportStep(chResourceCloudStorage),
		},
	})
}

// Test that a ClickHouse Cluster version and resources could be updated simultaneously.
func TestAccMDBClickHouseCluster_ClusterResources(t *testing.T) {
	var r clickhouse.Cluster
	chName := acctest.RandomWithPrefix("tf-clickhouse-cluster-resources")
	folderID := getExampleFolderID()
	bucketName := acctest.RandomWithPrefix("tf-test-clickhouse-bucket")
	rInt := acctest.RandInt()

	firstStep := &clickhouse.Resources{
		ResourcePresetId: "s2.micro",
		DiskTypeId:       "network-ssd",
		DiskSize:         10737418240,
	}

	secondStep := &clickhouse.Resources{
		ResourcePresetId: "s2.small",
		DiskTypeId:       "network-ssd",
		DiskSize:         17179869184,
	}

	thirdStepCluster := &clickhouse.Resources{
		ResourcePresetId: "s2.micro",
		DiskTypeId:       "network-ssd",
		DiskSize:         19327352832,
	}

	thirdStepZookeeper := &clickhouse.Resources{
		ResourcePresetId: "s2.micro",
		DiskTypeId:       "network-ssd",
		DiskSize:         10737418240,
	}

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckMDBClickHouseClusterDestroy,
		Steps: []resource.TestStep{
			// Create ClickHouse Cluster
			{
				Config: testAccMDBClickHouseClusterResources(chName, "Step 1", bucketName, rInt, chVersion, firstStep),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResource, "version", chVersion),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.resources.0.resource_preset_id", firstStep.ResourcePresetId),
					testAccCheckMDBClickHouseClusterHasResources(&r, firstStep.ResourcePresetId, firstStep.DiskTypeId, firstStep.DiskSize),
					testAccCheckCreatedAtAttr(chResource)),
			},
			mdbClickHouseClusterImportStep(chResource),
			// Update ClickHouse version and cluster resources
			{
				Config: testAccMDBClickHouseClusterResources(chName, "Step 2", bucketName, rInt, chUpdatedVersion, secondStep),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResource, "version", chUpdatedVersion),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.resources.0.resource_preset_id", secondStep.ResourcePresetId),
					testAccCheckMDBClickHouseClusterHasResources(&r, secondStep.ResourcePresetId, secondStep.DiskTypeId, secondStep.DiskSize),

					testAccCheckCreatedAtAttr(chResource)),
			},
			mdbClickHouseClusterImportStep(chResource),
			// Add host, creates implicit ZooKeeper subclusters
			{
				Config: testAccMDBClickHouseClusterResourceZookeepers(chName, "Step 3", bucketName, rInt, thirdStepCluster, thirdStepZookeeper),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 5),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),

					resource.TestCheckResourceAttr(chResource, "security_group_ids.#", "1"),
					resource.TestCheckResourceAttrSet(chResource, "host.0.fqdn"),
					resource.TestCheckResourceAttrSet(chResource, "host.1.fqdn"),
					testAccCheckMDBClickHouseClusterHasResources(&r, thirdStepCluster.ResourcePresetId, thirdStepCluster.DiskTypeId, thirdStepCluster.DiskSize),
					testAccCheckMDBClickHouseZooKeeperSubclusterHasResources(&r, thirdStepZookeeper.ResourcePresetId, thirdStepZookeeper.DiskTypeId, thirdStepZookeeper.DiskSize),
					testAccCheckCreatedAtAttr(chResource),
				),
			},
			mdbClickHouseClusterImportStep(chResource),
		},
	})
}

func TestAccMDBClickHouseCluster_UserSettings(t *testing.T) {
	t.Parallel()

	var r clickhouse.Cluster
	chName := acctest.RandomWithPrefix("tf-clickhouse")
	folderID := getExampleFolderID()
	bucketName := acctest.RandomWithPrefix("tf-test-clickhouse-bucket")
	rInt := acctest.RandInt()

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckMDBClickHouseClusterDestroy,
		Steps: []resource.TestStep{
			// Create ClickHouse Cluster with specify user settings
			{
				Config: testAccMDBClickHouseClusterConfigExpandUserParams(chName, "Step 1", "PRESTABLE", bucketName, rInt),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.max_concurrent_queries_for_user", "0"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.memory_profiler_step", "4194304"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.memory_profiler_sample_probability", "0"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.insert_null_as_default", "false"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.allow_suspicious_low_cardinality_types", "false"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.connect_timeout_with_failover", "50"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.allow_introspection_functions", "false"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert", "false"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert_threads", "16"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.wait_for_async_insert", "false"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.wait_for_async_insert_timeout", "1000"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert_max_data_size", "100000"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert_busy_timeout", "200"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert_stale_timeout", "1000"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.timeout_before_checking_execution_speed", "1000"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.cancel_http_readonly_queries_on_client_close", "false"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.flatten_nested", "false"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.max_http_get_redirects", "0"),
				),
			},
			mdbClickHouseClusterImportStep(chResource),
			{
				Config: testAccMDBClickHouseClusterConfigExpandUserParamsUpdated(chName, "Step 2", "PRESTABLE", bucketName, rInt),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.max_concurrent_queries_for_user", "1"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.memory_profiler_step", "4194301"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.memory_profiler_sample_probability", "1"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.insert_null_as_default", "true"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.allow_suspicious_low_cardinality_types", "true"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.connect_timeout_with_failover", "51"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.allow_introspection_functions", "true"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert", "true"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert_threads", "17"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.wait_for_async_insert", "true"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.wait_for_async_insert_timeout", "2000"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert_max_data_size", "100001"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert_busy_timeout", "201"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.async_insert_stale_timeout", "1001"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.timeout_before_checking_execution_speed", "2000"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.cancel_http_readonly_queries_on_client_close", "true"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.flatten_nested", "true"),
					resource.TestCheckResourceAttr(chResource, "user.0.settings.0.max_http_get_redirects", "1"),
				),
			},
			mdbClickHouseClusterImportStep(chResource),
		},
	})
}

func TestAccMDBClickHouseCluster_CheckClickhouseConfig(t *testing.T) {
	t.Parallel()

	var r clickhouse.Cluster
	chName := acctest.RandomWithPrefix("tf-clickhouse")
	folderID := getExampleFolderID()
	bucketName := acctest.RandomWithPrefix("tf-test-clickhouse-bucket")
	rInt := acctest.RandInt()

	configForFirstStep := &cfg.ClickhouseConfig{
		MergeTree: &cfg.ClickhouseConfig_MergeTree{
			ReplicatedDeduplicationWindow:                  &wrappers.Int64Value{Value: 1000},
			ReplicatedDeduplicationWindowSeconds:           &wrappers.Int64Value{Value: 1000},
			PartsToDelayInsert:                             &wrappers.Int64Value{Value: 110001},
			PartsToThrowInsert:                             &wrappers.Int64Value{Value: 11001},
			MaxReplicatedMergesInQueue:                     &wrappers.Int64Value{Value: 11000},
			NumberOfFreeEntriesInPoolToLowerMaxSizeOfMerge: &wrappers.Int64Value{Value: 15},
			MaxBytesToMergeAtMinSpaceInPool:                &wrappers.Int64Value{Value: 11000},
			MinBytesForWidePart:                            &wrappers.Int64Value{Value: 0},
			MinRowsForWidePart:                             &wrappers.Int64Value{Value: 0},
			TtlOnlyDropParts:                               &wrappers.BoolValue{Value: false},
		},
		Kafka: &cfg.ClickhouseConfig_Kafka{
			SecurityProtocol: cfg.ClickhouseConfig_Kafka_SECURITY_PROTOCOL_PLAINTEXT,
			SaslMechanism:    cfg.ClickhouseConfig_Kafka_SASL_MECHANISM_GSSAPI,
			SaslUsername:     "user1",
			SaslPassword:     "pass1",
		},
		KafkaTopics: []*cfg.ClickhouseConfig_KafkaTopic{
			{
				Name: "topic1",
				Settings: &cfg.ClickhouseConfig_Kafka{
					SecurityProtocol: cfg.ClickhouseConfig_Kafka_SECURITY_PROTOCOL_SSL,
					SaslMechanism:    cfg.ClickhouseConfig_Kafka_SASL_MECHANISM_SCRAM_SHA_256,
					SaslUsername:     "user2",
					SaslPassword:     "pass22",
				},
			},
		},
		Rabbitmq: &cfg.ClickhouseConfig_Rabbitmq{
			Username: "rabbit_user",
			Password: "rabbit_pass",
			Vhost:    "old_clickhouse",
		},
		Compression: []*cfg.ClickhouseConfig_Compression{
			{
				Method:           cfg.ClickhouseConfig_Compression_LZ4,
				MinPartSize:      1024,
				MinPartSizeRatio: 0.5,
			},
		},
		GraphiteRollup: []*cfg.ClickhouseConfig_GraphiteRollup{
			{
				Name: "rollup1",
				Patterns: []*cfg.ClickhouseConfig_GraphiteRollup_Pattern{
					{
						Regexp:   "abc",
						Function: "func1",
						Retention: []*cfg.ClickhouseConfig_GraphiteRollup_Pattern_Retention{
							{
								Age:       1000,
								Precision: 3,
							},
						},
					},
				},
			},
		},
		LogLevel:                    cfg.ClickhouseConfig_TRACE,
		MaxConnections:              &wrappers.Int64Value{Value: 512},
		MaxConcurrentQueries:        &wrappers.Int64Value{Value: 100},
		KeepAliveTimeout:            &wrappers.Int64Value{Value: 123000},
		UncompressedCacheSize:       &wrappers.Int64Value{Value: 8096},
		MarkCacheSize:               &wrappers.Int64Value{Value: 8096},
		MaxTableSizeToDrop:          &wrappers.Int64Value{Value: 1024},
		MaxPartitionSizeToDrop:      &wrappers.Int64Value{Value: 1024},
		Timezone:                    "UTC",
		GeobaseUri:                  "",
		QueryLogRetentionSize:       &wrappers.Int64Value{Value: 1024},
		QueryLogRetentionTime:       &wrappers.Int64Value{Value: 123000},
		QueryThreadLogEnabled:       &wrappers.BoolValue{Value: false},
		QueryThreadLogRetentionSize: &wrappers.Int64Value{Value: 1024},
		QueryThreadLogRetentionTime: &wrappers.Int64Value{Value: 123000},
		PartLogRetentionSize:        &wrappers.Int64Value{Value: 1024},
		PartLogRetentionTime:        &wrappers.Int64Value{Value: 123000},
		MetricLogEnabled:            &wrappers.BoolValue{Value: true},
		MetricLogRetentionSize:      &wrappers.Int64Value{Value: 1024},
		MetricLogRetentionTime:      &wrappers.Int64Value{Value: 123000},
		TraceLogEnabled:             &wrappers.BoolValue{Value: true},
		TraceLogRetentionSize:       &wrappers.Int64Value{Value: 1024},
		TraceLogRetentionTime:       &wrappers.Int64Value{Value: 123000},
		TextLogEnabled:              &wrappers.BoolValue{Value: true},
		TextLogRetentionSize:        &wrappers.Int64Value{Value: 1024},
		TextLogRetentionTime:        &wrappers.Int64Value{Value: 123000},
		TextLogLevel:                cfg.ClickhouseConfig_WARNING,
		BackgroundPoolSize:          &wrappers.Int64Value{Value: 16},
		BackgroundSchedulePoolSize:  &wrappers.Int64Value{Value: 32},
		BackgroundFetchesPoolSize:   &wrappers.Int64Value{Value: 8},
		DefaultDatabase:             &wrappers.StringValue{Value: "default"},
		TotalMemoryProfilerStep:     &wrappers.Int64Value{Value: 4194304},
	}

	configForSecondStep := &cfg.ClickhouseConfig{
		MergeTree: &cfg.ClickhouseConfig_MergeTree{
			ReplicatedDeduplicationWindow:                  &wrappers.Int64Value{Value: 100},
			ReplicatedDeduplicationWindowSeconds:           &wrappers.Int64Value{Value: 604800},
			PartsToDelayInsert:                             &wrappers.Int64Value{Value: 150},
			PartsToThrowInsert:                             &wrappers.Int64Value{Value: 12000},
			MaxReplicatedMergesInQueue:                     &wrappers.Int64Value{Value: 16},
			NumberOfFreeEntriesInPoolToLowerMaxSizeOfMerge: &wrappers.Int64Value{Value: 8},
			MaxBytesToMergeAtMinSpaceInPool:                &wrappers.Int64Value{Value: 1048576},
			MinBytesForWidePart:                            &wrappers.Int64Value{Value: 512},
			MinRowsForWidePart:                             &wrappers.Int64Value{Value: 16},
			TtlOnlyDropParts:                               &wrappers.BoolValue{Value: true},
		},
		Kafka: &cfg.ClickhouseConfig_Kafka{
			SecurityProtocol: cfg.ClickhouseConfig_Kafka_SECURITY_PROTOCOL_PLAINTEXT,
			SaslMechanism:    cfg.ClickhouseConfig_Kafka_SASL_MECHANISM_GSSAPI,
			SaslUsername:     "user1",
			SaslPassword:     "pass1",
		},
		KafkaTopics: []*cfg.ClickhouseConfig_KafkaTopic{
			{
				Name: "topic1",
				Settings: &cfg.ClickhouseConfig_Kafka{
					SecurityProtocol: cfg.ClickhouseConfig_Kafka_SECURITY_PROTOCOL_SSL,
					SaslMechanism:    cfg.ClickhouseConfig_Kafka_SASL_MECHANISM_SCRAM_SHA_256,
					SaslUsername:     "user2",
					SaslPassword:     "pass22",
				},
			},
			{
				Name: "topic2",
				Settings: &cfg.ClickhouseConfig_Kafka{
					SecurityProtocol: cfg.ClickhouseConfig_Kafka_SECURITY_PROTOCOL_SASL_PLAINTEXT,
					SaslMechanism:    cfg.ClickhouseConfig_Kafka_SASL_MECHANISM_PLAIN,
					SaslUsername:     "user2",
					SaslPassword:     "pass22",
				},
			},
		},
		Rabbitmq: &cfg.ClickhouseConfig_Rabbitmq{
			Username: "rabbit_user",
			Password: "rabbit_pass2",
			Vhost:    "clickhouse",
		},
		Compression: []*cfg.ClickhouseConfig_Compression{
			{
				Method:           cfg.ClickhouseConfig_Compression_LZ4,
				MinPartSize:      2024,
				MinPartSizeRatio: 0.3,
			},
			{
				Method:           cfg.ClickhouseConfig_Compression_ZSTD,
				MinPartSize:      4048,
				MinPartSizeRatio: 0.77,
			},
		},
		GraphiteRollup: []*cfg.ClickhouseConfig_GraphiteRollup{
			{
				Name: "rollup1",
				Patterns: []*cfg.ClickhouseConfig_GraphiteRollup_Pattern{
					{
						Regexp:   "abc",
						Function: "func1",
						Retention: []*cfg.ClickhouseConfig_GraphiteRollup_Pattern_Retention{
							{
								Age:       1000,
								Precision: 3,
							},
						},
					},
				},
			},
			{
				Name: "rollup2",
				Patterns: []*cfg.ClickhouseConfig_GraphiteRollup_Pattern{
					{
						Regexp:   "abc",
						Function: "func3",
						Retention: []*cfg.ClickhouseConfig_GraphiteRollup_Pattern_Retention{
							{
								Age:       3000,
								Precision: 7,
							},
						},
					},
				},
			},
		},
		LogLevel:                    cfg.ClickhouseConfig_WARNING,
		MaxConnections:              &wrappers.Int64Value{Value: 1024},
		MaxConcurrentQueries:        &wrappers.Int64Value{Value: 200},
		KeepAliveTimeout:            &wrappers.Int64Value{Value: 246000},
		UncompressedCacheSize:       &wrappers.Int64Value{Value: 16192},
		MarkCacheSize:               &wrappers.Int64Value{Value: 16192},
		MaxTableSizeToDrop:          &wrappers.Int64Value{Value: 2048},
		MaxPartitionSizeToDrop:      &wrappers.Int64Value{Value: 2048},
		Timezone:                    "UTC",
		GeobaseUri:                  "",
		QueryLogRetentionSize:       &wrappers.Int64Value{Value: 2048},
		QueryLogRetentionTime:       &wrappers.Int64Value{Value: 246000},
		QueryThreadLogEnabled:       &wrappers.BoolValue{Value: true},
		QueryThreadLogRetentionSize: &wrappers.Int64Value{Value: 2048},

		QueryThreadLogRetentionTime: &wrappers.Int64Value{Value: 246000},
		PartLogRetentionSize:        &wrappers.Int64Value{Value: 2048},
		PartLogRetentionTime:        &wrappers.Int64Value{Value: 246000},
		MetricLogEnabled:            &wrappers.BoolValue{Value: true},
		MetricLogRetentionSize:      &wrappers.Int64Value{Value: 2048},
		MetricLogRetentionTime:      &wrappers.Int64Value{Value: 246000},
		TraceLogEnabled:             &wrappers.BoolValue{Value: true},
		TraceLogRetentionSize:       &wrappers.Int64Value{Value: 2048},
		TraceLogRetentionTime:       &wrappers.Int64Value{Value: 246000},
		TextLogEnabled:              &wrappers.BoolValue{Value: true},
		TextLogRetentionSize:        &wrappers.Int64Value{Value: 2048},
		TextLogRetentionTime:        &wrappers.Int64Value{Value: 246000},
		TextLogLevel:                cfg.ClickhouseConfig_ERROR,
		BackgroundPoolSize:          &wrappers.Int64Value{Value: 32},
		BackgroundSchedulePoolSize:  &wrappers.Int64Value{Value: 64},
		BackgroundFetchesPoolSize:   &wrappers.Int64Value{Value: 16},
		DefaultDatabase:             &wrappers.StringValue{Value: "new_default"},
		TotalMemoryProfilerStep:     &wrappers.Int64Value{Value: 4194303},
	}

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckMDBClickHouseClusterDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccMDBClickHouseClusterConfig(chName, bucketName, "step 1", rInt, chVersion, configForFirstStep),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResource, "version", chVersion),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.log_level", "TRACE"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.max_connections", "512"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.max_concurrent_queries", "100"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.keep_alive_timeout", "123000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.uncompressed_cache_size", "8096"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.mark_cache_size", "8096"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.max_table_size_to_drop", "1024"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.max_partition_size_to_drop", "1024"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.timezone", "UTC"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.geobase_uri", ""),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_log_retention_size", "1024"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_log_retention_time", "123000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_thread_log_enabled", "false"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_thread_log_retention_size", "1024"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_thread_log_retention_time", "123000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.part_log_retention_size", "1024"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.part_log_retention_time", "123000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.metric_log_enabled", "true"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.metric_log_retention_size", "1024"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.metric_log_retention_time", "123000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.trace_log_enabled", "true"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.trace_log_retention_size", "1024"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.trace_log_retention_time", "123000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.text_log_enabled", "true"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.trace_log_retention_size", "1024"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.text_log_retention_time", "123000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.text_log_level", "WARNING"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.background_pool_size", "16"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.background_schedule_pool_size", "32"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.background_fetches_pool_size", "8"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.default_database", "default"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.total_memory_profiler_step", "4194304"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.replicated_deduplication_window", "1000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.replicated_deduplication_window_seconds", "1000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.parts_to_delay_insert", "110001"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.parts_to_throw_insert", "11001"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.max_replicated_merges_in_queue", "11000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.number_of_free_entries_in_pool_to_lower_max_size_of_merge", "15"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.max_bytes_to_merge_at_min_space_in_pool", "11000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.min_bytes_for_wide_part", "0"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.min_rows_for_wide_part", "0"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.ttl_only_drop_parts", "false"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka.0.security_protocol", "SECURITY_PROTOCOL_PLAINTEXT"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka.0.sasl_mechanism", "SASL_MECHANISM_GSSAPI"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka.0.sasl_username", "user1"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka.0.sasl_password", "pass1"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.#", "1"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.name", "topic1"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.settings.0.security_protocol", "SECURITY_PROTOCOL_SSL"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.settings.0.sasl_mechanism", "SASL_MECHANISM_SCRAM_SHA_256"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.settings.0.sasl_username", "user2"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.settings.0.sasl_password", "pass22"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.rabbitmq.0.username", "rabbit_user"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.rabbitmq.0.password", "rabbit_pass"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.rabbitmq.0.vhost", "old_clickhouse"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.graphite_rollup.#", "1"),

					testAccCheckCreatedAtAttr(chResource)),
			},
			mdbClickHouseClusterImportStep(chResource),
			{
				Config: testAccMDBClickHouseClusterConfig(chName, bucketName, "step 2", rInt, chVersion, configForSecondStep),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckMDBClickHouseClusterExists(chResource, &r, 1),
					resource.TestCheckResourceAttr(chResource, "name", chName),
					resource.TestCheckResourceAttr(chResource, "folder_id", folderID),
					resource.TestCheckResourceAttr(chResource, "version", chVersion),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.log_level", "WARNING"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.max_connections", "1024"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.max_concurrent_queries", "200"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.keep_alive_timeout", "246000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.uncompressed_cache_size", "16192"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.mark_cache_size", "16192"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.max_table_size_to_drop", "2048"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.max_partition_size_to_drop", "2048"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.timezone", "UTC"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.geobase_uri", ""),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_log_retention_size", "2048"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_log_retention_time", "246000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_thread_log_enabled", "true"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_thread_log_retention_size", "2048"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.query_thread_log_retention_time", "246000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.part_log_retention_size", "2048"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.part_log_retention_time", "246000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.metric_log_enabled", "true"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.metric_log_retention_size", "2048"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.metric_log_retention_time", "246000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.trace_log_enabled", "true"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.trace_log_retention_size", "2048"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.trace_log_retention_time", "246000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.text_log_enabled", "true"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.trace_log_retention_size", "2048"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.text_log_retention_time", "246000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.text_log_level", "ERROR"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.background_pool_size", "32"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.background_schedule_pool_size", "64"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.background_fetches_pool_size", "16"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.default_database", "new_default"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.total_memory_profiler_step", "4194303"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.replicated_deduplication_window", "100"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.replicated_deduplication_window_seconds", "604800"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.parts_to_delay_insert", "150"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.parts_to_throw_insert", "12000"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.max_replicated_merges_in_queue", "16"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.number_of_free_entries_in_pool_to_lower_max_size_of_merge", "8"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.max_bytes_to_merge_at_min_space_in_pool", "1048576"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.min_bytes_for_wide_part", "512"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.min_rows_for_wide_part", "16"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.merge_tree.0.ttl_only_drop_parts", "true"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka.0.security_protocol", "SECURITY_PROTOCOL_PLAINTEXT"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka.0.sasl_mechanism", "SASL_MECHANISM_GSSAPI"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka.0.sasl_username", "user1"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka.0.sasl_password", "pass1"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.#", "2"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.name", "topic1"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.settings.0.security_protocol", "SECURITY_PROTOCOL_SSL"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.settings.0.sasl_mechanism", "SASL_MECHANISM_SCRAM_SHA_256"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.settings.0.sasl_username", "user2"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.0.settings.0.sasl_password", "pass22"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.1.name", "topic2"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.1.settings.0.security_protocol", "SECURITY_PROTOCOL_SASL_PLAINTEXT"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.1.settings.0.sasl_mechanism", "SASL_MECHANISM_PLAIN"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.1.settings.0.sasl_username", "user2"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.kafka_topic.1.settings.0.sasl_password", "pass22"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.rabbitmq.0.username", "rabbit_user"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.rabbitmq.0.password", "rabbit_pass2"),
					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.rabbitmq.0.vhost", "clickhouse"),

					resource.TestCheckResourceAttr(chResource, "clickhouse.0.config.0.compression.#", "2"),

					testAccCheckCreatedAtAttr(chResource)),
			},
			mdbClickHouseClusterImportStep(chResource),
		},
	})
}

func testAccCheckMDBClickHouseClusterDestroy(s *terraform.State) error {
	config := testAccProvider.Meta().(*Config)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "yandex_mdb_clickhouse_cluster" {
			continue
		}

		_, err := config.sdk.MDB().Clickhouse().Cluster().Get(context.Background(), &clickhouse.GetClusterRequest{
			ClusterId: rs.Primary.ID,
		})

		if err == nil {
			return fmt.Errorf("ClickHouse Cluster still exists")
		}
	}

	return nil
}

func testAccCheckMDBClickHouseClusterExists(n string, r *clickhouse.Cluster, hosts int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Not found: %s", n)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		config := testAccProvider.Meta().(*Config)

		found, err := config.sdk.MDB().Clickhouse().Cluster().Get(context.Background(), &clickhouse.GetClusterRequest{
			ClusterId: rs.Primary.ID,
		})
		if err != nil {
			return err
		}

		if found.Id != rs.Primary.ID {
			return fmt.Errorf("ClickHouse Cluster not found")
		}

		*r = *found

		resp, err := config.sdk.MDB().Clickhouse().Cluster().ListHosts(context.Background(), &clickhouse.ListClusterHostsRequest{
			ClusterId: rs.Primary.ID,
			PageSize:  defaultMDBPageSize,
		})
		if err != nil {
			return err
		}

		if len(resp.Hosts) != hosts {
			return fmt.Errorf("Expected %d hosts, got %d", hosts, len(resp.Hosts))
		}

		return nil
	}
}

func testAccCheckMDBClickHouseClusterHasResources(r *clickhouse.Cluster, resourcePresetID string, diskType string, diskSize int64) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := r.Config.Clickhouse.Resources
		if rs.ResourcePresetId != resourcePresetID {
			return fmt.Errorf("Expected resource preset id '%s', got '%s'", resourcePresetID, rs.ResourcePresetId)
		}
		if rs.DiskTypeId != diskType {
			return fmt.Errorf("Expected disk type '%s', got '%s'", diskType, rs.DiskTypeId)
		}
		if rs.DiskSize != diskSize {
			return fmt.Errorf("Expected disk size '%d', got '%d'", diskSize, rs.DiskSize)
		}
		return nil
	}
}

func testAccCheckMDBClickHouseClusterHasShards(r *clickhouse.Cluster, shards []string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		config := testAccProvider.Meta().(*Config)

		resp, err := config.sdk.MDB().Clickhouse().Cluster().ListShards(context.Background(), &clickhouse.ListClusterShardsRequest{
			ClusterId: r.Id,
			PageSize:  defaultMDBPageSize,
		})
		if err != nil {
			return err
		}

		if len(resp.Shards) != len(shards) {
			return fmt.Errorf("Expected %d shards, got %d", len(shards), len(resp.Shards))
		}
		for _, s := range shards {
			found := false
			for _, rs := range resp.Shards {
				if s == rs.Name {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("Shard '%s' not found", s)
			}
		}
		return nil
	}
}

func testAccCheckMDBClickHouseClusterHasShardGroups(r *clickhouse.Cluster, shardGroups map[string][]string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		config := testAccProvider.Meta().(*Config)

		resp, err := config.sdk.MDB().Clickhouse().Cluster().ListShardGroups(context.Background(), &clickhouse.ListClusterShardGroupsRequest{
			ClusterId: r.Id,
			PageSize:  defaultMDBPageSize,
		})
		if err != nil {
			return err
		}

		if len(resp.ShardGroups) != len(shardGroups) {
			return fmt.Errorf("Expected %d shard groups, got %d", len(shardGroups), len(resp.ShardGroups))
		}
		for name, shards := range shardGroups {
			found := false
			for _, rs := range resp.ShardGroups {
				if name == rs.Name {
					found = true
					if !reflect.DeepEqual(shards, rs.ShardNames) {
						return fmt.Errorf("Shards in group %s not match, expexted %s, got %s", name, fmt.Sprint(shards), fmt.Sprint(rs.ShardNames))
					}
				}
			}
			if !found {
				return fmt.Errorf("Shard group '%s' not found", s)
			}
		}
		return nil
	}
}

func testAccCheckMDBClickHouseZooKeeperSubclusterHasResources(r *clickhouse.Cluster, resourcePresetID string, diskType string, diskSize int64) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := r.Config.Zookeeper.Resources
		if rs.ResourcePresetId != resourcePresetID {
			return fmt.Errorf("Expected resource preset id '%s', got '%s'", resourcePresetID, rs.ResourcePresetId)
		}
		if rs.DiskTypeId != diskType {
			return fmt.Errorf("Expected disk type '%s', got '%s'", diskType, rs.DiskTypeId)
		}
		if rs.DiskSize != diskSize {
			return fmt.Errorf("Expected disk size '%d', got '%d'", diskSize, rs.DiskSize)
		}
		return nil
	}
}

func testAccCheckMDBClickHouseClusterHasUsers(r string, perms map[string][]string, settings map[string]map[string]interface{},
	quotas map[string][]map[string]interface{}) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[r]
		if !ok {
			return fmt.Errorf("Not found: %s", r)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		config := testAccProvider.Meta().(*Config)

		resp, err := config.sdk.MDB().Clickhouse().User().List(context.Background(), &clickhouse.ListUsersRequest{
			ClusterId: rs.Primary.ID,
			PageSize:  defaultMDBPageSize,
		})
		if err != nil {
			return err
		}
		users := resp.Users

		if len(users) != len(perms) {
			return fmt.Errorf("Expected %d users, found %d", len(perms), len(users))
		}

		for _, u := range users {
			ps, ok := perms[u.Name]
			if !ok {
				return fmt.Errorf("Unexpected user: %s", u.Name)
			}

			ups := []string{}
			for _, p := range u.Permissions {
				ups = append(ups, p.DatabaseName)
			}

			sort.Strings(ps)
			sort.Strings(ups)
			if fmt.Sprintf("%v", ps) != fmt.Sprintf("%v", ups) {
				return fmt.Errorf("User %s has wrong permissions, %v. Expected %v", u.Name, ups, ps)
			}

			ss, ok := settings[u.Name]
			if !ok {
				ss = map[string]interface{}{}
			}

			flatSettings := flattenClickHouseUserSettings(u.Settings)
			for key, setting := range flatSettings {
				s, ok := ss[key]
				if !ok {
					switch setting.(type) {
					case int:
						s = 0
					case bool:
						s = false
					case string:
						s = "unspecified"
					default:
						return fmt.Errorf("User %s has unexpected setting '%s'='%v'", u.Name, key, setting)
					}
				}
				if fmt.Sprintf("%v", s) != fmt.Sprintf("%v", setting) {
					return fmt.Errorf("User %s has incorrect setting '%s'='%v', expected '%v'", u.Name, key, setting, s)
				}
				delete(ss, key)
			}

			if len(ss) != 0 {
				return fmt.Errorf("User %s has not expected settings %v", u.Name, ss)
			}

			qs, ok := quotas[u.Name]
			if !ok {
				qs = []map[string]interface{}{}
			}

			qsm := map[int]map[string]interface{}{}

			for _, q := range qs {
				duration, ok := q["interval_duration"].(int)
				if !ok {
					return fmt.Errorf("Wrong test: user %s has wrong quota test data %v", u.Name, q)
				}
				qsm[duration] = q
			}

			for _, quota := range u.Quotas {
				flatQuota := flattenClickHouseUserQuota(quota)
				duration := int(quota.IntervalDuration.Value)
				q, ok := qsm[duration]
				if !ok {
					return fmt.Errorf("User %s has unexpected quota %v", u.Name, quota)
				}
				if fmt.Sprintf("%v", q) != fmt.Sprintf("%v", flatQuota) {
					return fmt.Errorf("User %s has wrong quota %v, expected %v", u.Name, flatQuota, q)
				}
				delete(qsm, duration)
			}

			if len(qsm) != 0 {
				return fmt.Errorf("User %s has not expected quotas %v", u.Name, qsm)
			}
		}

		return nil
	}
}

func testAccCheckMDBClickHouseClusterHasDatabases(r string, databases []string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[r]
		if !ok {
			return fmt.Errorf("Not found: %s", r)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		config := testAccProvider.Meta().(*Config)

		resp, err := config.sdk.MDB().Clickhouse().Database().List(context.Background(), &clickhouse.ListDatabasesRequest{
			ClusterId: rs.Primary.ID,
			PageSize:  defaultMDBPageSize,
		})
		if err != nil {
			return err
		}
		dbs := []string{}
		for _, d := range resp.Databases {
			dbs = append(dbs, d.Name)
		}

		if len(dbs) != len(databases) {
			return fmt.Errorf("Expected %d dbs, found %d", len(databases), len(dbs))
		}

		sort.Strings(dbs)
		sort.Strings(databases)
		if fmt.Sprintf("%v", dbs) != fmt.Sprintf("%v", databases) {
			return fmt.Errorf("Cluster has wrong databases, %v. Expected %v", dbs, databases)
		}

		return nil
	}
}

func testAccCheckMDBClickHouseClusterContainsLabel(r *clickhouse.Cluster, key string, value string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		v, ok := r.Labels[key]
		if !ok {
			return fmt.Errorf("Expected label with key '%s' not found", key)
		}
		if v != value {
			return fmt.Errorf("Incorrect label value for key '%s': expected '%s' but found '%s'", key, value, v)
		}
		return nil
	}
}

func testAccCheckMDBClickHouseClusterHasFormatSchemas(r string, targetSchemas map[string]map[string]string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[r]
		if !ok {
			return fmt.Errorf("Not found: %s", r)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		config := testAccProvider.Meta().(*Config)

		resp, err := config.sdk.MDB().Clickhouse().FormatSchema().List(context.Background(), &clickhouse.ListFormatSchemasRequest{
			ClusterId: rs.Primary.ID,
			PageSize:  defaultMDBPageSize,
		})
		if err != nil {
			return err
		}
		formatSchemas := resp.FormatSchemas

		if len(formatSchemas) != len(targetSchemas) {
			return fmt.Errorf("expected %d format schemas, found %d", len(formatSchemas), len(targetSchemas))
		}

		for _, s := range formatSchemas {
			ts, ok := targetSchemas[s.Name]
			if !ok {
				return fmt.Errorf("unexpected format schema: %s", s.Name)
			}

			if s.Type.String() != ts["type"] {
				return fmt.Errorf("format schema %s has wrong type, %v. expected %v", s.Name, s.Type.String(), ts["type"])
			}

			if s.Uri != ts["uri"] {
				return fmt.Errorf("format schema %s has wrong uri, %v. expected %v", s.Name, s.Uri, ts["uri"])
			}
		}

		return nil
	}
}

func testAccCheckMDBClickHouseClusterHasMlModels(r string, targetModels map[string]map[string]string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[r]
		if !ok {
			return fmt.Errorf("Not found: %s", r)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		config := testAccProvider.Meta().(*Config)

		resp, err := config.sdk.MDB().Clickhouse().MlModel().List(context.Background(), &clickhouse.ListMlModelsRequest{
			ClusterId: rs.Primary.ID,
			PageSize:  defaultMDBPageSize,
		})
		if err != nil {
			return err
		}
		mlModels := resp.MlModels

		if len(mlModels) != len(targetModels) {
			return fmt.Errorf("expected %d ml models, found %d", len(mlModels), len(targetModels))
		}

		for _, m := range mlModels {
			tm, ok := targetModels[m.Name]
			if !ok {
				return fmt.Errorf("unexpected ml model: %s", m.Name)
			}

			if m.Type.String() != tm["type"] {
				return fmt.Errorf("ml model %s has wrong type, %v. expected %v", m.Name, m.Type.String(), tm["type"])
			}

			if m.Uri != tm["uri"] {
				return fmt.Errorf("ml model %s has wrong uri, %v. expected %v", m.Name, m.Uri, tm["uri"])
			}
		}

		return nil
	}
}

const clickHouseVPCDependencies = `
resource "yandex_vpc_network" "mdb-ch-test-net" {}

resource "yandex_vpc_subnet" "mdb-ch-test-subnet-a" {
  zone           = "ru-central1-a"
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  v4_cidr_blocks = ["10.1.0.0/24"]
}

resource "yandex_vpc_subnet" "mdb-ch-test-subnet-b" {
  zone           = "ru-central1-b"
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  v4_cidr_blocks = ["10.2.0.0/24"]
}

resource "yandex_vpc_subnet" "mdb-ch-test-subnet-c" {
  zone           = "ru-central1-c"
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  v4_cidr_blocks = ["10.3.0.0/24"]
}

resource "yandex_vpc_security_group" "mdb-ch-test-sg-x" {
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  ingress {
    protocol          = "ANY"
    description       = "Allow incoming traffic from members of the same security group"
    from_port         = 0
    to_port           = 65535
    v4_cidr_blocks    = ["0.0.0.0/0"]
  }
  egress {
    protocol          = "ANY"
    description       = "Allow outgoing traffic to members of the same security group"
    from_port         = 0
    to_port           = 65535
    v4_cidr_blocks    = ["0.0.0.0/0"]
  }
}

resource "yandex_vpc_security_group" "mdb-ch-test-sg-y" {
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  
  ingress {
    protocol          = "ANY"
    description       = "Allow incoming traffic from members of the same security group"
    from_port         = 0
    to_port           = 65535
    v4_cidr_blocks    = ["0.0.0.0/0"]
  }
  egress {
    protocol          = "ANY"
    description       = "Allow outgoing traffic to members of the same security group"
    from_port         = 0
    to_port           = 65535
    v4_cidr_blocks    = ["0.0.0.0/0"]
  }
}
`

func clickhouseObjectStorageDependencies(bucket string, randInt int) string {
	return testAccCommonIamDependenciesEditorConfig(randInt) + fmt.Sprintf(`
resource "yandex_storage_bucket" "tmp_bucket" {
  bucket = "%s"
  acl    = "public-read"

  access_key = yandex_iam_service_account_static_access_key.sa-key.access_key
  secret_key = yandex_iam_service_account_static_access_key.sa-key.secret_key
}

resource "yandex_storage_object" "test_capnp" {
  bucket = yandex_storage_bucket.tmp_bucket.bucket

  access_key = yandex_iam_service_account_static_access_key.sa-key.access_key
  secret_key = yandex_iam_service_account_static_access_key.sa-key.secret_key

  key     = "test.capnp"
  content = "# This is a comment."

  acl = "public-read"

  depends_on = [
    yandex_storage_bucket.tmp_bucket
  ]
}

resource "yandex_storage_object" "test_capnp2" {
  bucket = yandex_storage_bucket.tmp_bucket.bucket

  access_key = yandex_iam_service_account_static_access_key.sa-key.access_key
  secret_key = yandex_iam_service_account_static_access_key.sa-key.secret_key

  key     = "test2.capnp"
  content = "# This is a comment."
}

resource "yandex_storage_object" "test_proto" {
  bucket = yandex_storage_bucket.tmp_bucket.bucket

  access_key = yandex_iam_service_account_static_access_key.sa-key.access_key
  secret_key = yandex_iam_service_account_static_access_key.sa-key.secret_key

  key     = "test.proto"
  content = "# This is a comment."
}

resource "yandex_storage_object" "test_ml_model" {
  bucket = yandex_storage_bucket.tmp_bucket.bucket

  access_key = yandex_iam_service_account_static_access_key.sa-key.access_key
  secret_key = yandex_iam_service_account_static_access_key.sa-key.secret_key

  key     = "train.csv"
  content = "a,b,c"
}
`, bucket)
}

func testAccMDBClickHouseClusterConfigMain(name, desc, environment string, deletionProtection bool, bucket string, randInt int, maintenanceWindow string) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "foo" {
  depends_on = [
    yandex_storage_object.test_ml_model
  ]

  name           = "%s"
  description    = "%s"
  environment    = "%s"
  version        = "%s"
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password = "strong_password"

  labels = {
    test_key = "test_value"
  }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }
  }

  database {
    name = "testdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
    settings {
      add_http_cors_header                               = true
      allow_ddl                                          = false
      compile                                            = false
      compile_expressions                                = false
      connect_timeout                                    = 42000
      count_distinct_implementation                      = "uniq_combined_64"
      distinct_overflow_mode                             = "unspecified"
      distributed_aggregation_memory_efficient           = false
      distributed_ddl_task_timeout                       = 0
      distributed_product_mode                           = "unspecified"
      empty_result_for_aggregation_by_empty_set          = false
      enable_http_compression                            = false
      fallback_to_stale_replicas_for_distributed_queries = false
      force_index_by_date                                = false
      force_primary_key                                  = false
      group_by_overflow_mode                             = "unspecified"
      group_by_two_level_threshold                       = 0
      group_by_two_level_threshold_bytes                 = 0
      http_connection_timeout                            = 0
      http_headers_progress_interval                     = 0
      http_receive_timeout                               = 0
      http_send_timeout                                  = 0
      input_format_defaults_for_omitted_fields           = false
      input_format_values_interpret_expressions          = false
      insert_quorum                                      = 0
      insert_quorum_timeout                              = 0
      join_overflow_mode                                 = "unspecified"
      join_use_nulls                                     = false
      joined_subquery_requires_alias                     = false
      low_cardinality_allow_in_native_format             = false
      max_ast_depth                                      = 0
      max_ast_elements                                   = 0
      max_block_size                                     = 0
      max_bytes_before_external_group_by                 = 0
      max_bytes_before_external_sort                     = 0
      max_bytes_in_distinct                              = 0
      max_bytes_in_join                                  = 0
      max_bytes_in_set                                   = 0
      max_bytes_to_read                                  = 0
      max_bytes_to_sort                                  = 0
      max_bytes_to_transfer                              = 0
      max_columns_to_read                                = 0
      max_execution_time                                 = 0
      max_expanded_ast_elements                          = 0
      max_insert_block_size                              = 0
      max_memory_usage                                   = 0
      max_memory_usage_for_user                          = 0
      max_network_bandwidth                              = 0
      max_network_bandwidth_for_user                     = 0
      max_query_size                                     = 0
      max_replica_delay_for_distributed_queries          = 0
      max_result_bytes                                   = 0
      max_result_rows                                    = 0
      max_rows_in_distinct                               = 0
      max_rows_in_join                                   = 0
      max_rows_in_set                                    = 0
      max_rows_to_group_by                               = 0
      max_rows_to_read                                   = 0
      max_rows_to_sort                                   = 0
      max_rows_to_transfer                               = 0
      max_temporary_columns                              = 0
      max_temporary_non_const_columns                    = 0
      max_threads                                        = 0
      merge_tree_max_bytes_to_use_cache                  = 0
      merge_tree_max_rows_to_use_cache                   = 0
      merge_tree_min_bytes_for_concurrent_read           = 0
      merge_tree_min_rows_for_concurrent_read            = 0
      min_bytes_to_use_direct_io                         = 0
      min_count_to_compile                               = 0
      min_count_to_compile_expression                    = 0
      min_execution_speed                                = 0
      min_execution_speed_bytes                          = 0
      min_insert_block_size_bytes                        = 0
      min_insert_block_size_rows                         = 0
      output_format_json_quote_64bit_integers            = false
      output_format_json_quote_denormals                 = false
      priority                                           = 0
      quota_mode                                         = "unspecified"
      read_overflow_mode                                 = "unspecified"
      readonly                                           = 0
      receive_timeout                                    = 0
      replication_alter_partitions_sync                  = 0
      result_overflow_mode                               = "unspecified"
      select_sequential_consistency                      = false
      send_progress_in_http_headers                      = false
      send_timeout                                       = 0
      set_overflow_mode                                  = "unspecified"
      skip_unavailable_shards                            = false
      sort_overflow_mode                                 = "unspecified"
      timeout_overflow_mode                              = "unspecified"
      transfer_overflow_mode                             = "unspecified"
      transform_null_in                                  = false
      use_uncompressed_cache                             = false
    }
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}"]
  service_account_id = "${yandex_iam_service_account.sa.id}"

  maintenance_window {
    %s
  }

  access {
	web_sql       = true
	data_lens     = true
	metrika       = true
	serverless    = true
	data_transfer = true
	yandex_query  = true
  }

  deletion_protection = %t
}
`, name, desc, environment, chVersion, maintenanceWindow, deletionProtection)
}

func testAccMDBClickHouseClusterConfigUpdated(name, desc, bucket string, randInt int) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "foo" {
  name           = "%s"
  description    = "%s"
  environment    = "PRESTABLE"
  version        = "%s"
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password = "strong_password"

  labels = {
    new_key = "new_value"
  }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }
  }

  database {
    name = "testdb"
  }

  database {
    name = "newdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
    settings {
      add_http_cors_header                               = true
      allow_ddl                                          = false
      compile                                            = false
      compile_expressions                                = false
      connect_timeout                                    = 44000
      count_distinct_implementation                      = "uniq_combined_64"
      distinct_overflow_mode                             = "unspecified"
      distributed_aggregation_memory_efficient           = false
      distributed_ddl_task_timeout                       = 0
      distributed_product_mode                           = "unspecified"
      empty_result_for_aggregation_by_empty_set          = false
      enable_http_compression                            = false
      fallback_to_stale_replicas_for_distributed_queries = false
      force_index_by_date                                = false
      force_primary_key                                  = false
      group_by_overflow_mode                             = "unspecified"
      group_by_two_level_threshold                       = 0
      group_by_two_level_threshold_bytes                 = 0
      http_connection_timeout                            = 0
      http_headers_progress_interval                     = 0
      http_receive_timeout                               = 0
      http_send_timeout                                  = 0
      input_format_defaults_for_omitted_fields           = false
      input_format_values_interpret_expressions          = false
      insert_quorum                                      = 0
      insert_quorum_timeout                              = 0
      join_overflow_mode                                 = "unspecified"
      join_use_nulls                                     = false
      joined_subquery_requires_alias                     = false
      low_cardinality_allow_in_native_format             = false
      max_ast_depth                                      = 0
      max_ast_elements                                   = 0
      max_block_size                                     = 0
      max_bytes_before_external_group_by                 = 0
      max_bytes_before_external_sort                     = 0
      max_bytes_in_distinct                              = 0
      max_bytes_in_join                                  = 0
      max_bytes_in_set                                   = 0
      max_bytes_to_read                                  = 0
      max_bytes_to_sort                                  = 0
      max_bytes_to_transfer                              = 0
      max_columns_to_read                                = 0
      max_execution_time                                 = 0
      max_expanded_ast_elements                          = 0
      max_insert_block_size                              = 0
      max_memory_usage                                   = 0
      max_memory_usage_for_user                          = 0
      max_network_bandwidth                              = 0
      max_network_bandwidth_for_user                     = 0
      max_query_size                                     = 0
      max_replica_delay_for_distributed_queries          = 0
      max_result_bytes                                   = 0
      max_result_rows                                    = 0
      max_rows_in_distinct                               = 0
      max_rows_in_join                                   = 0
      max_rows_in_set                                    = 0
      max_rows_to_group_by                               = 0
      max_rows_to_read                                   = 0
      max_rows_to_sort                                   = 0
      max_rows_to_transfer                               = 0
      max_temporary_columns                              = 0
      max_temporary_non_const_columns                    = 0
      max_threads                                        = 0
      merge_tree_max_bytes_to_use_cache                  = 0
      merge_tree_max_rows_to_use_cache                   = 0
      merge_tree_min_bytes_for_concurrent_read           = 0
      merge_tree_min_rows_for_concurrent_read            = 0
      min_bytes_to_use_direct_io                         = 0
      min_count_to_compile                               = 0
      min_count_to_compile_expression                    = 0
      min_execution_speed                                = 0
      min_execution_speed_bytes                          = 0
      min_insert_block_size_bytes                        = 0
      min_insert_block_size_rows                         = 0
      output_format_json_quote_64bit_integers            = false
      output_format_json_quote_denormals                 = false
      priority                                           = 0
      quota_mode                                         = "unspecified"
      read_overflow_mode                                 = "unspecified"
      readonly                                           = 0
      receive_timeout                                    = 0
      replication_alter_partitions_sync                  = 0
      result_overflow_mode                               = "unspecified"
      select_sequential_consistency                      = false
      send_progress_in_http_headers                      = false
      send_timeout                                       = 0
      set_overflow_mode                                  = "unspecified"
      skip_unavailable_shards                            = false
      sort_overflow_mode                                 = "unspecified"
      timeout_overflow_mode                              = "unspecified"
      transfer_overflow_mode                             = "unspecified"
      transform_null_in                                  = false
      use_uncompressed_cache                             = false
    }
  }

  user {
    name     = "mary"
    password = "qwerty123"
    permission {
      database_name = "newdb"
    }
    permission {
      database_name = "testdb"
    }
    settings {
      add_http_cors_header                               = false
      allow_ddl                                          = false
      compile                                            = false
      compile_expressions                                = false
      connect_timeout                                    = 0
      count_distinct_implementation                      = "unspecified"
      distinct_overflow_mode                             = "unspecified"
      distributed_aggregation_memory_efficient           = false
      distributed_ddl_task_timeout                       = 0
      distributed_product_mode                           = "unspecified"
      empty_result_for_aggregation_by_empty_set          = false
      enable_http_compression                            = false
      fallback_to_stale_replicas_for_distributed_queries = false
      force_index_by_date                                = false
      force_primary_key                                  = false
      group_by_overflow_mode                             = "unspecified"
      group_by_two_level_threshold                       = 0
      group_by_two_level_threshold_bytes                 = 0
      http_connection_timeout                            = 0
      http_headers_progress_interval                     = 0
      http_receive_timeout                               = 0
      http_send_timeout                                  = 0
      input_format_defaults_for_omitted_fields           = false
      input_format_values_interpret_expressions          = false
      insert_quorum                                      = 0
      insert_quorum_timeout                              = 0
      join_overflow_mode                                 = "unspecified"
      join_use_nulls                                     = false
      joined_subquery_requires_alias                     = false
      low_cardinality_allow_in_native_format             = false
      max_ast_depth                                      = 0
      max_ast_elements                                   = 0
      max_block_size                                     = 0
      max_bytes_before_external_group_by                 = 0
      max_bytes_before_external_sort                     = 0
      max_bytes_in_distinct                              = 0
      max_bytes_in_join                                  = 0
      max_bytes_in_set                                   = 0
      max_bytes_to_read                                  = 0
      max_bytes_to_sort                                  = 0
      max_bytes_to_transfer                              = 0
      max_columns_to_read                                = 0
      max_execution_time                                 = 0
      max_expanded_ast_elements                          = 0
      max_insert_block_size                              = 0
      max_memory_usage                                   = 0
      max_memory_usage_for_user                          = 0
      max_network_bandwidth                              = 0
      max_network_bandwidth_for_user                     = 0
      max_query_size                                     = 0
      max_replica_delay_for_distributed_queries          = 0
      max_result_bytes                                   = 0
      max_result_rows                                    = 0
      max_rows_in_distinct                               = 0
      max_rows_in_join                                   = 0
      max_rows_in_set                                    = 0
      max_rows_to_group_by                               = 0
      max_rows_to_read                                   = 0
      max_rows_to_sort                                   = 0
      max_rows_to_transfer                               = 0
      max_temporary_columns                              = 0
      max_temporary_non_const_columns                    = 0
      max_threads                                        = 0
      merge_tree_max_bytes_to_use_cache                  = 0
      merge_tree_max_rows_to_use_cache                   = 0
      merge_tree_min_bytes_for_concurrent_read           = 0
      merge_tree_min_rows_for_concurrent_read            = 0
      min_bytes_to_use_direct_io                         = 0
      min_count_to_compile                               = 0
      min_count_to_compile_expression                    = 0
      min_execution_speed                                = 0
      min_execution_speed_bytes                          = 0
      min_insert_block_size_bytes                        = 0
      min_insert_block_size_rows                         = 0
      output_format_json_quote_64bit_integers            = false
      output_format_json_quote_denormals                 = false
      priority                                           = 0
      quota_mode                                         = "unspecified"
      read_overflow_mode                                 = "unspecified"
      readonly                                           = 0
      receive_timeout                                    = 0
      replication_alter_partitions_sync                  = 0
      result_overflow_mode                               = "unspecified"
      select_sequential_consistency                      = false
      send_progress_in_http_headers                      = false
      send_timeout                                       = 0
      set_overflow_mode                                  = "unspecified"
      skip_unavailable_shards                            = false
      sort_overflow_mode                                 = "unspecified"
      timeout_overflow_mode                              = "unspecified"
      transfer_overflow_mode                             = "unspecified"
      transform_null_in                                  = false
      use_uncompressed_cache                             = false
    }
    quota {
      interval_duration = 3600000
      queries           = 1000
    }
    quota {
      interval_duration = 79800000
      queries           = 5000
    }
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}", "${yandex_vpc_security_group.mdb-ch-test-sg-y.id}"]

  format_schema {
    name = "test_schema"
    type = "FORMAT_SCHEMA_TYPE_CAPNPROTO"
    uri  = "%s/${yandex_storage_bucket.tmp_bucket.bucket}/test.capnp"
  }

  ml_model {
    name = "test_model"
    type = "ML_MODEL_TYPE_CATBOOST"
    uri  = "%s/${yandex_storage_bucket.tmp_bucket.bucket}/train.csv"
  }

  maintenance_window {
    type = "ANYTIME"
  }

  cloud_storage {
    enabled = true
  }

  deletion_protection = false
}
`, name, desc, chVersion, StorageEndpointUrl, StorageEndpointUrl)
}

func testAccMDBClickHouseClusterConfigUser(name, desc, bucket string, randInt int) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "foo" {
  name                     = "%s"
  description              = "%s"
  environment              = "PRESTABLE"
  version                  = "%s"
  network_id               = "${yandex_vpc_network.mdb-ch-test-net.id}"
  copy_schema_on_new_hosts = true

  labels = {
    new_key = "new_value"
  }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }
  }

  database {
    name = "testdb"
  }

  database {
    name = "newdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
    settings {
      add_http_cors_header                               = true
      allow_ddl                                          = false
      compile                                            = false
      compile_expressions                                = false
      connect_timeout                                    = 44000
      count_distinct_implementation                      = "uniq_hll_12"
      distinct_overflow_mode                             = "unspecified"
      distributed_aggregation_memory_efficient           = false
      distributed_ddl_task_timeout                       = 0
      distributed_product_mode                           = "unspecified"
      empty_result_for_aggregation_by_empty_set          = false
      enable_http_compression                            = false
      fallback_to_stale_replicas_for_distributed_queries = false
      force_index_by_date                                = false
      force_primary_key                                  = false
      group_by_overflow_mode                             = "unspecified"
      group_by_two_level_threshold                       = 0
      group_by_two_level_threshold_bytes                 = 0
      http_connection_timeout                            = 0
      http_headers_progress_interval                     = 0
      http_receive_timeout                               = 0
      http_send_timeout                                  = 0
      input_format_defaults_for_omitted_fields           = false
      input_format_values_interpret_expressions          = false
      insert_quorum                                      = 0
      insert_quorum_timeout                              = 0
      join_overflow_mode                                 = "unspecified"
      join_use_nulls                                     = false
      joined_subquery_requires_alias                     = false
      low_cardinality_allow_in_native_format             = false
      max_ast_depth                                      = 0
      max_ast_elements                                   = 0
      max_block_size                                     = 0
      max_bytes_before_external_group_by                 = 0
      max_bytes_before_external_sort                     = 0
      max_bytes_in_distinct                              = 0
      max_bytes_in_join                                  = 0
      max_bytes_in_set                                   = 0
      max_bytes_to_read                                  = 0
      max_bytes_to_sort                                  = 0
      max_bytes_to_transfer                              = 0
      max_columns_to_read                                = 0
      max_execution_time                                 = 0
      max_expanded_ast_elements                          = 0
      max_insert_block_size                              = 0
      max_memory_usage                                   = 0
      max_memory_usage_for_user                          = 0
      max_network_bandwidth                              = 0
      max_network_bandwidth_for_user                     = 0
      max_query_size                                     = 0
      max_replica_delay_for_distributed_queries          = 0
      max_result_bytes                                   = 0
      max_result_rows                                    = 0
      max_rows_in_distinct                               = 0
      max_rows_in_join                                   = 0
      max_rows_in_set                                    = 0
      max_rows_to_group_by                               = 0
      max_rows_to_read                                   = 0
      max_rows_to_sort                                   = 0
      max_rows_to_transfer                               = 0
      max_temporary_columns                              = 0
      max_temporary_non_const_columns                    = 0
      max_threads                                        = 0
      merge_tree_max_bytes_to_use_cache                  = 0
      merge_tree_max_rows_to_use_cache                   = 0
      merge_tree_min_bytes_for_concurrent_read           = 0
      merge_tree_min_rows_for_concurrent_read            = 0
      min_bytes_to_use_direct_io                         = 0
      min_count_to_compile                               = 0
      min_count_to_compile_expression                    = 0
      min_execution_speed                                = 0
      min_execution_speed_bytes                          = 0
      min_insert_block_size_bytes                        = 0
      min_insert_block_size_rows                         = 0
      output_format_json_quote_64bit_integers            = false
      output_format_json_quote_denormals                 = false
      priority                                           = 0
      quota_mode                                         = "unspecified"
      read_overflow_mode                                 = "unspecified"
      readonly                                           = 0
      receive_timeout                                    = 0
      replication_alter_partitions_sync                  = 0
      result_overflow_mode                               = "unspecified"
      select_sequential_consistency                      = false
      send_progress_in_http_headers                      = false
      send_timeout                                       = 0
      set_overflow_mode                                  = "unspecified"
      skip_unavailable_shards                            = false
      sort_overflow_mode                                 = "unspecified"
      timeout_overflow_mode                              = "unspecified"
      transfer_overflow_mode                             = "unspecified"
      transform_null_in                                  = false
      use_uncompressed_cache                             = false
    }
  }

  user {
    name     = "mary"
    password = "qwerty123"
    permission {
      database_name = "newdb"
    }
    permission {
      database_name = "testdb"
    }
    settings {
      add_http_cors_header                               = false
      allow_ddl                                          = false
      compile                                            = false
      compile_expressions                                = false
      connect_timeout                                    = 0
      count_distinct_implementation                      = "unspecified"
      distinct_overflow_mode                             = "unspecified"
      distributed_aggregation_memory_efficient           = false
      distributed_ddl_task_timeout                       = 0
      distributed_product_mode                           = "unspecified"
      empty_result_for_aggregation_by_empty_set          = false
      enable_http_compression                            = false
      fallback_to_stale_replicas_for_distributed_queries = false
      force_index_by_date                                = false
      force_primary_key                                  = false
      group_by_overflow_mode                             = "unspecified"
      group_by_two_level_threshold                       = 0
      group_by_two_level_threshold_bytes                 = 0
      http_connection_timeout                            = 0
      http_headers_progress_interval                     = 0
      http_receive_timeout                               = 0
      http_send_timeout                                  = 0
      input_format_defaults_for_omitted_fields           = false
      input_format_values_interpret_expressions          = false
      insert_quorum                                      = 0
      insert_quorum_timeout                              = 0
      join_overflow_mode                                 = "unspecified"
      join_use_nulls                                     = false
      joined_subquery_requires_alias                     = false
      low_cardinality_allow_in_native_format             = false
      max_ast_depth                                      = 0
      max_ast_elements                                   = 0
      max_block_size                                     = 0
      max_bytes_before_external_group_by                 = 0
      max_bytes_before_external_sort                     = 0
      max_bytes_in_distinct                              = 0
      max_bytes_in_join                                  = 0
      max_bytes_in_set                                   = 0
      max_bytes_to_read                                  = 0
      max_bytes_to_sort                                  = 0
      max_bytes_to_transfer                              = 0
      max_columns_to_read                                = 0
      max_execution_time                                 = 0
      max_expanded_ast_elements                          = 0
      max_insert_block_size                              = 0
      max_memory_usage                                   = 0
      max_memory_usage_for_user                          = 0
      max_network_bandwidth                              = 0
      max_network_bandwidth_for_user                     = 0
      max_query_size                                     = 0
      max_replica_delay_for_distributed_queries          = 0
      max_result_bytes                                   = 0
      max_result_rows                                    = 0
      max_rows_in_distinct                               = 0
      max_rows_in_join                                   = 0
      max_rows_in_set                                    = 0
      max_rows_to_group_by                               = 0
      max_rows_to_read                                   = 0
      max_rows_to_sort                                   = 0
      max_rows_to_transfer                               = 0
      max_temporary_columns                              = 0
      max_temporary_non_const_columns                    = 0
      max_threads                                        = 0
      merge_tree_max_bytes_to_use_cache                  = 0
      merge_tree_max_rows_to_use_cache                   = 0
      merge_tree_min_bytes_for_concurrent_read           = 0
      merge_tree_min_rows_for_concurrent_read            = 0
      min_bytes_to_use_direct_io                         = 0
      min_count_to_compile                               = 0
      min_count_to_compile_expression                    = 0
      min_execution_speed                                = 0
      min_execution_speed_bytes                          = 0
      min_insert_block_size_bytes                        = 0
      min_insert_block_size_rows                         = 0
      output_format_json_quote_64bit_integers            = false
      output_format_json_quote_denormals                 = false
      priority                                           = 0
      quota_mode                                         = "unspecified"
      read_overflow_mode                                 = "unspecified"
      readonly                                           = 0
      receive_timeout                                    = 0
      replication_alter_partitions_sync                  = 0
      result_overflow_mode                               = "unspecified"
      select_sequential_consistency                      = false
      send_progress_in_http_headers                      = false
      send_timeout                                       = 0
      set_overflow_mode                                  = "unspecified"
      skip_unavailable_shards                            = false
      sort_overflow_mode                                 = "unspecified"
      timeout_overflow_mode                              = "unspecified"
      transfer_overflow_mode                             = "unspecified"
      transform_null_in                                  = false
      use_uncompressed_cache                             = false
    }
    quota {
      interval_duration = 3600000
      queries           = 2000
    }
    quota {
      interval_duration = 7200000
      queries           = 3000
    }
    quota {
      interval_duration = 79800000
      queries           = 5000
    }
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}"]

  format_schema {
    name = "test_schema"
    type = "FORMAT_SCHEMA_TYPE_CAPNPROTO"
    uri  = "%s/${yandex_storage_bucket.tmp_bucket.bucket}/test2.capnp"
  }

  format_schema {
    name = "test_schema2"
    type = "FORMAT_SCHEMA_TYPE_PROTOBUF"
    uri  = "%s/${yandex_storage_bucket.tmp_bucket.bucket}/test.proto"
  }

  ml_model {
    name = "test_model"
    type = "ML_MODEL_TYPE_CATBOOST"
    uri  = "%s/${yandex_storage_bucket.tmp_bucket.bucket}/train.csv"
  }

  ml_model {
    name = "test_model2"
    type = "ML_MODEL_TYPE_CATBOOST"
    uri  = "%s/${yandex_storage_bucket.tmp_bucket.bucket}/train.csv"
  }

  cloud_storage {
    enabled = true
  }
}
`, name, desc, chVersion, StorageEndpointUrl, StorageEndpointUrl, StorageEndpointUrl, StorageEndpointUrl)
}

func testAccMDBClickHouseClusterResourceZookeepers(name, desc, bucket string, randInt int, resourcesCluster, resourcesZookeeper *clickhouse.Resources) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "foo" {
  name                     = "%s"
  description              = "%s"
  environment              = "PRESTABLE"
  version                  = "%s"
  network_id               = "${yandex_vpc_network.mdb-ch-test-net.id}"
  copy_schema_on_new_hosts = true

  clickhouse {
    # resources
	%s
  }

  zookeeper {
    # resources
	%s
  }

  database {
    name = "testdb"
  }

  database {
    name = "newdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-b"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-b.id}"
  }

  host {
    type      = "ZOOKEEPER"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  host {
    type      = "ZOOKEEPER"
    zone      = "ru-central1-b"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-b.id}"
  }

  host {
    type      = "ZOOKEEPER"
    zone      = "ru-central1-c"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-c.id}"
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}"]
}
`, name, desc, chVersion,
		buildResources(resourcesCluster),
		buildResources(resourcesZookeeper))
}

func testAccMDBClickHouseClusterConfigSharded(name string, clusterDiskSize int, firstShardDiskSize, secondShardDiskSize int, bucket string, randInt int) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "bar" {
  name           = "%s"
  description    = "ClickHouse Sharded Cluster Terraform Test"
  environment    = "PRESTABLE"
  network_id     = yandex_vpc_network.mdb-ch-test-net.id
  admin_password = "strong_password"

  clickhouse {
    resources {
      resource_preset_id = "s3-c2-m8"
      disk_type_id       = "network-ssd"
      disk_size          = %d
    }
  }

  database {
    name = "testdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
  }

  shard {
	name = "shard1"
	weight = 11
	resources {
      resource_preset_id = "s3-c4-m16"
      disk_type_id       = "network-ssd"
      disk_size          = %d
    }
  }

  shard {
	name = "shard2"
	weight = 22
	resources {
      resource_preset_id = "s3-c2-m8"
      disk_type_id       = "network-ssd"
      disk_size          = %d
    }
  }

  host {
    type             = "CLICKHOUSE"
    zone             = "ru-central1-a"
    subnet_id        = yandex_vpc_subnet.mdb-ch-test-subnet-a.id
    shard_name       = "shard1"
    assign_public_ip = false
  }

  host {
    type             = "CLICKHOUSE"
    zone             = "ru-central1-b"
    subnet_id        = yandex_vpc_subnet.mdb-ch-test-subnet-b.id
    shard_name       = "shard2"
    assign_public_ip = false
  }

  shard_group {
    name        = "test_group"
    description = "test shard group"
    shard_names = [
      "shard1",
      "shard2",
    ]
  }

  shard_group {
    name        = "test_group_2"
    description = "shard group to delete"
    shard_names = [
      "shard1",
    ]
  }
}
`, name, clusterDiskSize, firstShardDiskSize, secondShardDiskSize)
}

func testAccMDBClickHouseClusterConfigShardedUpdated(name string, clusterDiskSize int, bucket string, randInt int) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "bar" {
  name           = "%s"
  description    = "ClickHouse Sharded Cluster Terraform Test"
  environment    = "PRESTABLE"
  network_id     = yandex_vpc_network.mdb-ch-test-net.id
  admin_password = "strong_password"

  clickhouse {
    resources {
      resource_preset_id = "s3-c2-m8"
      disk_type_id       = "network-ssd"
      disk_size          = %d
    }
  }

  database {
    name = "testdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
  }

  host {
    type             = "CLICKHOUSE"
    zone             = "ru-central1-a"
    subnet_id        = yandex_vpc_subnet.mdb-ch-test-subnet-a.id
    shard_name       = "shard1"
    assign_public_ip = true
  }

  host {
    type             = "CLICKHOUSE"
    zone             = "ru-central1-c"
    subnet_id        = yandex_vpc_subnet.mdb-ch-test-subnet-c.id
    shard_name       = "shard3"
    assign_public_ip = true
  }

  shard {
	name = "shard1"
	weight = 110
  }

  shard {
	name = "shard3"
	weight = 330
  }

  shard_group {
    name        = "test_group"
    description = "updated shard group"
    shard_names = [
      "shard1",
      "shard3",
    ]
  }

  shard_group {
    name        = "test_group_3"
    description = "new shard group"
    shard_names = [
      "shard1",
    ]
  }

}
`, name, clusterDiskSize)
}

func testAccMDBClickHouseClusterConfigSqlManaged(name, desc, bucket string, randInt int) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "foo" {
  depends_on = [
    yandex_storage_object.test_ml_model
  ]

  name                    = "%s"
  description             = "%s"
  environment             = "PRESTABLE"
  network_id              = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password          = "strong_password"
  sql_user_management     = true
  sql_database_management = true

  labels = {
    test_key = "test_value"
   }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}"]
}
`, name, desc)
}

func testAccMDBClickHouseClusterConfigDefaultCloudStorage(name, desc, bucket string, randInt int) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "cloud" {
  depends_on = [
    yandex_storage_object.test_ml_model
  ]

  name                    = "%s"
  description             = "%s"
  environment             = "PRESTABLE"
  network_id              = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password          = "strong_password"
  version                 = "%s"

  labels = {
    test_key = "test_value"
   }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }
  }
  
  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  database {
    name = "testdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
    settings {
      add_http_cors_header                               = true
      allow_ddl                                          = false
      compile                                            = false
      compile_expressions                                = false
      connect_timeout                                    = 44000
      count_distinct_implementation                      = "uniq_hll_12"
      distinct_overflow_mode                             = "unspecified"
      distributed_aggregation_memory_efficient           = false
      distributed_ddl_task_timeout                       = 0
      distributed_product_mode                           = "unspecified"
      empty_result_for_aggregation_by_empty_set          = false
      enable_http_compression                            = false
      fallback_to_stale_replicas_for_distributed_queries = false
      force_index_by_date                                = false
      force_primary_key                                  = false
      group_by_overflow_mode                             = "unspecified"
      group_by_two_level_threshold                       = 0
      group_by_two_level_threshold_bytes                 = 0
      http_connection_timeout                            = 0
      http_headers_progress_interval                     = 0
      http_receive_timeout                               = 0
      http_send_timeout                                  = 0
      input_format_defaults_for_omitted_fields           = false
      input_format_values_interpret_expressions          = false
      insert_quorum                                      = 0
      insert_quorum_timeout                              = 0
      join_overflow_mode                                 = "unspecified"
      join_use_nulls                                     = false
      joined_subquery_requires_alias                     = false
      low_cardinality_allow_in_native_format             = false
      max_ast_depth                                      = 0
      max_ast_elements                                   = 0
      max_block_size                                     = 0
      max_bytes_before_external_group_by                 = 0
      max_bytes_before_external_sort                     = 0
      max_bytes_in_distinct                              = 0
      max_bytes_in_join                                  = 0
      max_bytes_in_set                                   = 0
      max_bytes_to_read                                  = 0
      max_bytes_to_sort                                  = 0
      max_bytes_to_transfer                              = 0
      max_columns_to_read                                = 0
      max_execution_time                                 = 0
      max_expanded_ast_elements                          = 0
      max_insert_block_size                              = 0
      max_memory_usage                                   = 0
      max_memory_usage_for_user                          = 0
      max_network_bandwidth                              = 0
      max_network_bandwidth_for_user                     = 0
      max_query_size                                     = 0
      max_replica_delay_for_distributed_queries          = 0
      max_result_bytes                                   = 0
      max_result_rows                                    = 0
      max_rows_in_distinct                               = 0
      max_rows_in_join                                   = 0
      max_rows_in_set                                    = 0
      max_rows_to_group_by                               = 0
      max_rows_to_read                                   = 0
      max_rows_to_sort                                   = 0
      max_rows_to_transfer                               = 0
      max_temporary_columns                              = 0
      max_temporary_non_const_columns                    = 0
      max_threads                                        = 0
      merge_tree_max_bytes_to_use_cache                  = 0
      merge_tree_max_rows_to_use_cache                   = 0
      merge_tree_min_bytes_for_concurrent_read           = 0
      merge_tree_min_rows_for_concurrent_read            = 0
      min_bytes_to_use_direct_io                         = 0
      min_count_to_compile                               = 0
      min_count_to_compile_expression                    = 0
      min_execution_speed                                = 0
      min_execution_speed_bytes                          = 0
      min_insert_block_size_bytes                        = 0
      min_insert_block_size_rows                         = 0
      output_format_json_quote_64bit_integers            = false
      output_format_json_quote_denormals                 = false
      priority                                           = 0
      quota_mode                                         = "unspecified"
      read_overflow_mode                                 = "unspecified"
      readonly                                           = 0
      receive_timeout                                    = 0
      replication_alter_partitions_sync                  = 0
      result_overflow_mode                               = "unspecified"
      select_sequential_consistency                      = false
      send_progress_in_http_headers                      = false
      send_timeout                                       = 0
      set_overflow_mode                                  = "unspecified"
      skip_unavailable_shards                            = false
      sort_overflow_mode                                 = "unspecified"
      timeout_overflow_mode                              = "unspecified"
      transfer_overflow_mode                             = "unspecified"
      transform_null_in                                  = false
      use_uncompressed_cache                             = false
    }
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}"]
}
`, name, desc, chVersion)
}

func testAccMDBClickHouseClusterConfigCloudStorage(name, desc, bucket string, randInt int, enabled bool, moveFactor float64, dataCacheEnabled bool, dataCacheMaxSize int64) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "cloud" {
  depends_on = [
    yandex_storage_object.test_ml_model
  ]

  name                    = "%s"
  description             = "%s"
  environment             = "PRESTABLE"
  network_id              = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password          = "strong_password"
  version                 = "%s"

  labels = {
    test_key = "test_value"
   }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }
  }
  
  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  database {
    name = "testdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
    settings {
      add_http_cors_header                               = true
      allow_ddl                                          = false
      compile                                            = false
      compile_expressions                                = false
      connect_timeout                                    = 44000
      count_distinct_implementation                      = "uniq_hll_12"
      distinct_overflow_mode                             = "unspecified"
      distributed_aggregation_memory_efficient           = false
      distributed_ddl_task_timeout                       = 0
      distributed_product_mode                           = "unspecified"
      empty_result_for_aggregation_by_empty_set          = false
      enable_http_compression                            = false
      fallback_to_stale_replicas_for_distributed_queries = false
      force_index_by_date                                = false
      force_primary_key                                  = false
      group_by_overflow_mode                             = "unspecified"
      group_by_two_level_threshold                       = 0
      group_by_two_level_threshold_bytes                 = 0
      http_connection_timeout                            = 0
      http_headers_progress_interval                     = 0
      http_receive_timeout                               = 0
      http_send_timeout                                  = 0
      input_format_defaults_for_omitted_fields           = false
      input_format_values_interpret_expressions          = false
      insert_quorum                                      = 0
      insert_quorum_timeout                              = 0
      join_overflow_mode                                 = "unspecified"
      join_use_nulls                                     = false
      joined_subquery_requires_alias                     = false
      low_cardinality_allow_in_native_format             = false
      max_ast_depth                                      = 0
      max_ast_elements                                   = 0
      max_block_size                                     = 0
      max_bytes_before_external_group_by                 = 0
      max_bytes_before_external_sort                     = 0
      max_bytes_in_distinct                              = 0
      max_bytes_in_join                                  = 0
      max_bytes_in_set                                   = 0
      max_bytes_to_read                                  = 0
      max_bytes_to_sort                                  = 0
      max_bytes_to_transfer                              = 0
      max_columns_to_read                                = 0
      max_execution_time                                 = 0
      max_expanded_ast_elements                          = 0
      max_insert_block_size                              = 0
      max_memory_usage                                   = 0
      max_memory_usage_for_user                          = 0
      max_network_bandwidth                              = 0
      max_network_bandwidth_for_user                     = 0
      max_query_size                                     = 0
      max_replica_delay_for_distributed_queries          = 0
      max_result_bytes                                   = 0
      max_result_rows                                    = 0
      max_rows_in_distinct                               = 0
      max_rows_in_join                                   = 0
      max_rows_in_set                                    = 0
      max_rows_to_group_by                               = 0
      max_rows_to_read                                   = 0
      max_rows_to_sort                                   = 0
      max_rows_to_transfer                               = 0
      max_temporary_columns                              = 0
      max_temporary_non_const_columns                    = 0
      max_threads                                        = 0
      merge_tree_max_bytes_to_use_cache                  = 0
      merge_tree_max_rows_to_use_cache                   = 0
      merge_tree_min_bytes_for_concurrent_read           = 0
      merge_tree_min_rows_for_concurrent_read            = 0
      min_bytes_to_use_direct_io                         = 0
      min_count_to_compile                               = 0
      min_count_to_compile_expression                    = 0
      min_execution_speed                                = 0
      min_execution_speed_bytes                          = 0
      min_insert_block_size_bytes                        = 0
      min_insert_block_size_rows                         = 0
      output_format_json_quote_64bit_integers            = false
      output_format_json_quote_denormals                 = false
      priority                                           = 0
      quota_mode                                         = "unspecified"
      read_overflow_mode                                 = "unspecified"
      readonly                                           = 0
      receive_timeout                                    = 0
      replication_alter_partitions_sync                  = 0
      result_overflow_mode                               = "unspecified"
      select_sequential_consistency                      = false
      send_progress_in_http_headers                      = false
      send_timeout                                       = 0
      set_overflow_mode                                  = "unspecified"
      skip_unavailable_shards                            = false
      sort_overflow_mode                                 = "unspecified"
      timeout_overflow_mode                              = "unspecified"
      transfer_overflow_mode                             = "unspecified"
      transform_null_in                                  = false
      use_uncompressed_cache                             = false
    }
  }

  cloud_storage {
    enabled = %t
	move_factor = %f
	data_cache_enabled = %t
	data_cache_max_size = %d
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}"]
}
`, name, desc, chVersion, enabled, moveFactor, dataCacheEnabled, dataCacheMaxSize)
}

func testAccMDBClickHouseClusterConfigEmbeddedKeeper(name, desc, bucket string, randInt int) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "keeper" {
  depends_on = [
    yandex_storage_object.test_ml_model
  ]

  name        = "%s"
  description = "%s"
  environment = "PRESTABLE"
  network_id  = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password = "strong_password"
  sql_user_management     = true
  sql_database_management = true
  embedded_keeper = true

  labels = {
    test_key = "test_value"
  }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}"]
}
`, name, desc)
}

func testAccMDBClickHouseClusterResources(name, desc, bucket string, randInt int, version string, resources *clickhouse.Resources) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "foo"{
  name           = "%s"
  description    = "%s"
  environment    = "PRESTABLE"
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password = "strong_password"
  version        = "%s"

  labels = {
    test_key = "test_value"
  }

  clickhouse {
    # resources 
	%s
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }
}
`, name, desc, version, buildResources(resources))
}

func testAccMDBClickHouseClusterConfig(name, bucket, desc string, randInt int, version string, config *cfg.ClickhouseConfig) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "foo"{
  name           = "%s"
  description    = "%s"
  environment    = "PRESTABLE"
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password = "strong_password"
  version        = "%s"

  labels = {
    test_key = "test_value"
  }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }

	# config
	%s
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }
}
`, name, desc, version, buildClickhouseConfig(config))
}

func buildResources(resources *clickhouse.Resources) string {
	return fmt.Sprintf(`
resources {
      resource_preset_id = "%s"
      disk_type_id       = "%s"
      disk_size          = %d
    }
`,
		resources.ResourcePresetId,
		resources.DiskTypeId,
		toGigabytes(resources.DiskSize))
}

func buildClickhouseConfig(config *cfg.ClickhouseConfig) string {
	return fmt.Sprintf(`
config {
      	log_level		                = "%s"
		max_connections                 = %d
		max_concurrent_queries          = %d
		keep_alive_timeout              = %d
		uncompressed_cache_size         = %d
		mark_cache_size                 = %d
		max_table_size_to_drop          = %d
		max_partition_size_to_drop      = %d
		timezone                        = "%s"
		geobase_uri                     = "%s"
		query_log_retention_size        = %d
		query_log_retention_time        = %d
		query_thread_log_enabled        = %t
		query_thread_log_retention_size = %d
		query_thread_log_retention_time = %d
		part_log_retention_size         = %d
		part_log_retention_time         = %d
		metric_log_enabled              = %t
		metric_log_retention_size       = %d
		metric_log_retention_time       = %d
		trace_log_enabled               = %t
		trace_log_retention_size        = %d
		trace_log_retention_time        = %d
		text_log_enabled                = %t
		text_log_retention_size         = %d
		text_log_retention_time         = %d
		text_log_level                  = "%s"
		background_pool_size            = %d
		background_schedule_pool_size   = %d
		background_fetches_pool_size 	= %d
		default_database 				= "%s"
		total_memory_profiler_step 		= %d

		# merge_tree
		%s

		# kafka
		%s

		# kafka_topics
		%s

		# rabbitmq
		%s

		# compression
		%s

		# graphite_rollup
		%s
    }
`,
		config.LogLevel.String(),
		config.MaxConnections.GetValue(),
		config.MaxConcurrentQueries.GetValue(),
		config.KeepAliveTimeout.GetValue(),
		config.UncompressedCacheSize.GetValue(),
		config.MarkCacheSize.GetValue(),
		config.MaxTableSizeToDrop.GetValue(),
		config.MaxPartitionSizeToDrop.GetValue(),
		config.Timezone,
		config.GeobaseUri,
		config.QueryLogRetentionSize.GetValue(),
		config.QueryLogRetentionTime.GetValue(),
		config.QueryThreadLogEnabled.GetValue(),
		config.QueryThreadLogRetentionSize.GetValue(),
		config.QueryThreadLogRetentionTime.GetValue(),
		config.PartLogRetentionSize.GetValue(),
		config.PartLogRetentionTime.GetValue(),
		config.MetricLogEnabled.GetValue(),
		config.MetricLogRetentionSize.GetValue(),
		config.MetricLogRetentionTime.GetValue(),
		config.TextLogEnabled.GetValue(),
		config.TextLogRetentionSize.GetValue(),
		config.TextLogRetentionTime.GetValue(),
		config.TextLogEnabled.GetValue(),
		config.TextLogRetentionSize.GetValue(),
		config.TextLogRetentionTime.GetValue(),
		config.TextLogLevel.String(),
		config.BackgroundPoolSize.GetValue(),
		config.BackgroundSchedulePoolSize.GetValue(),
		config.BackgroundFetchesPoolSize.GetValue(),
		config.DefaultDatabase.GetValue(),
		config.TotalMemoryProfilerStep.GetValue(),
		buildConfigForMergeTree(config.MergeTree),
		buildConfigForKafka(config.Kafka),
		buildConfigForKafkaTopics(config.KafkaTopics),
		buildConfigForRabbitmq(config.Rabbitmq),
		buildConfigForCompression(config.Compression),
		buildGraphiteRollup(config.GraphiteRollup),
	)
}

func buildConfigForMergeTree(mergeTree *cfg.ClickhouseConfig_MergeTree) string {
	return fmt.Sprintf(`
merge_tree {
			replicated_deduplication_window                           = %d
			replicated_deduplication_window_seconds                   = %d
			parts_to_delay_insert                                     = %d
			parts_to_throw_insert                                     = %d
			max_replicated_merges_in_queue                            = %d
			number_of_free_entries_in_pool_to_lower_max_size_of_merge = %d
			max_bytes_to_merge_at_min_space_in_pool                   = %d
			min_bytes_for_wide_part 								  = %d
            min_rows_for_wide_part 									  = %d
            ttl_only_drop_parts 									  = %t
		}
`,
		mergeTree.ReplicatedDeduplicationWindow.GetValue(),
		mergeTree.ReplicatedDeduplicationWindowSeconds.GetValue(),
		mergeTree.PartsToDelayInsert.GetValue(),
		mergeTree.PartsToThrowInsert.GetValue(),
		mergeTree.MaxReplicatedMergesInQueue.GetValue(),
		mergeTree.NumberOfFreeEntriesInPoolToLowerMaxSizeOfMerge.GetValue(),
		mergeTree.MaxBytesToMergeAtMinSpaceInPool.GetValue(),
		mergeTree.MinBytesForWidePart.GetValue(),
		mergeTree.MinRowsForWidePart.GetValue(),
		mergeTree.TtlOnlyDropParts.GetValue())
}

func buildConfigForKafka(kafka *cfg.ClickhouseConfig_Kafka) string {
	return fmt.Sprintf(`
kafka {
			security_protocol = "%s"
			sasl_mechanism    = "%s"
			sasl_username     = "%s"
			sasl_password     = "%s"
		}
`,
		kafka.SecurityProtocol.String(),
		kafka.SaslMechanism.String(),
		kafka.SaslUsername,
		kafka.SaslPassword)
}

func buildConfigForKafkaTopics(topics []*cfg.ClickhouseConfig_KafkaTopic) string {
	var result string
	for _, rawTopic := range topics {
		result += fmt.Sprintf(`
kafka_topic {
	name = "%s"
	settings {
		security_protocol = "%s"
		sasl_mechanism    = "%s"
		sasl_username     = "%s"
		sasl_password     = "%s"
	}
}
`,
			rawTopic.Name,
			rawTopic.Settings.SecurityProtocol.String(),
			rawTopic.Settings.SaslMechanism.String(),
			rawTopic.Settings.SaslUsername,
			rawTopic.Settings.SaslPassword)
	}
	return result
}

func buildConfigForRabbitmq(rabbitmq *cfg.ClickhouseConfig_Rabbitmq) string {
	return fmt.Sprintf(`
rabbitmq {
        username = "%s"
        password = "%s"
		vhost 	 = "%s"
}
`,
		rabbitmq.Username,
		rabbitmq.Password,
		rabbitmq.Vhost)
}

func buildConfigForCompression(compression []*cfg.ClickhouseConfig_Compression) string {
	var result string
	for _, v := range compression {
		result += fmt.Sprintf(`
compression {
        method 				= "%s"
        min_part_size 		= %d
		min_part_size_ratio = %f
}
`,
			v.Method.String(),
			v.MinPartSize,
			v.MinPartSizeRatio)
	}
	return result
}

func buildGraphiteRollup(graphiteRollup []*cfg.ClickhouseConfig_GraphiteRollup) string {
	var result string
	for _, v := range graphiteRollup {
		result += fmt.Sprintf(`
graphite_rollup {
        name = "%s"
        pattern {
          regexp   = "%s"
          function = "%s"
          retention {
            age       = %d
            precision = %d
          }
        }
}
`,
			v.Name,
			v.Patterns[0].Regexp,
			v.Patterns[0].Function,
			v.Patterns[0].Retention[0].Age,
			v.Patterns[0].Retention[0].Precision)
	}
	return result
}

func testAccMDBClickHouseClusterConfigExpandUserParams(name, desc, environment string, bucket string, randInt int) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "foo" {
  depends_on = [
    yandex_storage_object.test_ml_model
  ]

  name           = "%s"
  description    = "%s"
  environment    = "%s"
  version        = "%s"
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password = "strong_password"

  labels = {
    test_key = "test_value"
  }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }
  }

  database {
    name = "testdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
    settings {
	  max_concurrent_queries_for_user					 = 0
	  memory_profiler_step 								 = 4194304
	  memory_profiler_sample_probability				 = 0
	  insert_null_as_default							 = false
 	  allow_suspicious_low_cardinality_types			 = false
	  connect_timeout_with_failover						 = 50
	  allow_introspection_functions						 = false
	  async_insert										 = false
	  async_insert_threads								 = 16
	  wait_for_async_insert								 = false
 	  wait_for_async_insert_timeout						 = 1000
	  async_insert_max_data_size						 = 100000
	  async_insert_busy_timeout							 = 200
	  async_insert_stale_timeout						 = 1000
	  timeout_before_checking_execution_speed			 = 1000
	  cancel_http_readonly_queries_on_client_close		 = false
	  flatten_nested									 = false
	  max_http_get_redirects							 = 0
    }
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}"]
  service_account_id = "${yandex_iam_service_account.sa.id}"
}
`, name, desc, environment, chVersion)
}

func testAccMDBClickHouseClusterConfigExpandUserParamsUpdated(name, desc, environment string, bucket string, randInt int) string {
	return fmt.Sprintf(clickHouseVPCDependencies+clickhouseObjectStorageDependencies(bucket, randInt)+`
resource "yandex_mdb_clickhouse_cluster" "foo" {
  depends_on = [
    yandex_storage_object.test_ml_model
  ]

  name           = "%s"
  description    = "%s"
  environment    = "%s"
  version        = "%s"
  network_id     = "${yandex_vpc_network.mdb-ch-test-net.id}"
  admin_password = "strong_password"

  labels = {
    test_key = "test_value"
  }

  clickhouse {
    resources {
      resource_preset_id = "s2.micro"
      disk_type_id       = "network-ssd"
      disk_size          = 16
    }
  }

  database {
    name = "testdb"
  }

  user {
    name     = "john"
    password = "password"
    permission {
      database_name = "testdb"
    }
    settings {
	  max_concurrent_queries_for_user					 = 1
	  memory_profiler_step 								 = 4194301
	  memory_profiler_sample_probability				 = 1
	  insert_null_as_default							 = true
 	  allow_suspicious_low_cardinality_types			 = true
	  connect_timeout_with_failover						 = 51
	  allow_introspection_functions						 = true
	  async_insert										 = true
	  async_insert_threads								 = 17
	  wait_for_async_insert								 = true
 	  wait_for_async_insert_timeout						 = 2000
	  async_insert_max_data_size						 = 100001
	  async_insert_busy_timeout							 = 201
	  async_insert_stale_timeout						 = 1001
	  timeout_before_checking_execution_speed			 = 2000
	  cancel_http_readonly_queries_on_client_close		 = true
	  flatten_nested									 = true
	  max_http_get_redirects							 = 1
    }
  }

  host {
    type      = "CLICKHOUSE"
    zone      = "ru-central1-a"
    subnet_id = "${yandex_vpc_subnet.mdb-ch-test-subnet-a.id}"
  }

  security_group_ids = ["${yandex_vpc_security_group.mdb-ch-test-sg-x.id}"]
  service_account_id = "${yandex_iam_service_account.sa.id}"
}
`, name, desc, environment, chVersion)
}
