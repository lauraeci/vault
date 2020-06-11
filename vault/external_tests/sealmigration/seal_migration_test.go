package sealmigration

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/go-test/deep"

	"github.com/hashicorp/go-hclog"
	wrapping "github.com/hashicorp/go-kms-wrapping"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/helper/testhelpers"
	sealhelper "github.com/hashicorp/vault/helper/testhelpers/seal"
	"github.com/hashicorp/vault/helper/testhelpers/teststorage"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/sdk/helper/logging"
	"github.com/hashicorp/vault/vault"
)

const (
	numTestCores = 5
	keyShares    = 3
	keyThreshold = 3
)

type testFunc func(t *testing.T, logger hclog.Logger, storage teststorage.ReusableStorage, basePort int)

func testVariousBackends(t *testing.T, tf testFunc, includeRaft bool) {

	logger := logging.NewVaultLogger(hclog.Debug).Named(t.Name())

	//t.Run("inmem", func(t *testing.T) {
	//	t.Parallel()

	//	logger := logger.Named("inmem")
	//	storage, cleanup := teststorage.MakeReusableStorage(
	//		t, logger, teststorage.MakeInmemBackend(t, logger))
	//	defer cleanup()
	//	tf(t, logger, storage, 51000)
	//})

	//t.Run("file", func(t *testing.T) {
	//	t.Parallel()

	//	logger := logger.Named("file")
	//	storage, cleanup := teststorage.MakeReusableStorage(
	//		t, logger, teststorage.MakeFileBackend(t, logger))
	//	defer cleanup()
	//	tf(t, logger, storage, 52000)
	//})

	//t.Run("consul", func(t *testing.T) {
	//	t.Parallel()

	//	logger := logger.Named("consul")
	//	storage, cleanup := teststorage.MakeReusableStorage(
	//		t, logger, teststorage.MakeConsulBackend(t, logger))
	//	defer cleanup()
	//	tf(t, logger, storage, 53000)
	//})

	if includeRaft {
		t.Run("raft", func(t *testing.T) {
			t.Parallel()

			logger := logger.Named("raft")
			storage, cleanup := teststorage.MakeReusableRaftStorage(t, logger, numTestCores)
			defer cleanup()
			tf(t, logger, storage, 54000)
		})
	}
}

// TestSealMigration_ShamirToTransit_Pre14 tests shamir-to-transit seal
// migration, using the pre-1.4 method of bring down the whole cluster to do
// the migration.
func TestSealMigration_ShamirToTransit_Pre14(t *testing.T) {
	// Note that we do not test integrated raft storage since this is
	// a pre-1.4 test.
	testVariousBackends(t, testSealMigrationShamirToTransit_Pre14, false)
}

func testSealMigrationShamirToTransit_Pre14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) {

	// Initialize the backend using shamir
	cluster, _, cleanup := initializeShamir(t, logger, storage, basePort)
	rootToken, barrierKeys := cluster.RootToken, cluster.BarrierKeys
	cluster.EnsureCoresSealed(t)
	cleanup()

	// Create the transit server.
	tss := sealhelper.NewTransitSealServer(t)
	defer func() {
		tss.EnsureCoresSealed(t)
		tss.Cleanup()
	}()
	tss.MakeKey(t, "transit-seal-key")

	// Migrate the backend from shamir to transit.  Note that the barrier keys
	// are now the recovery keys.
	transitSeal := migrateFromShamirToTransit_Pre14(t, logger, storage, basePort, tss, rootToken, barrierKeys)

	// Run the backend with transit.
	runTransit(t, logger, storage, basePort, rootToken, transitSeal)
}

func migrateFromShamirToTransit_Pre14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	tss *sealhelper.TransitSealServer, rootToken string, recoveryKeys [][]byte,
) vault.Seal {

	var baseClusterPort = basePort + 10

	var transitSeal vault.Seal

	var conf = vault.CoreConfig{
		Logger: logger.Named("migrateFromShamirToTransit"),
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
		SkipInit:              true,
		// N.B. Providing a transit seal puts us in migration mode.
		SealFunc: func() vault.Seal {
			transitSeal = tss.MakeSeal(t, "transit-seal-key")
			return transitSeal
		},
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	defer func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}()

	leader := cluster.Cores[0]
	client := leader.Client
	client.SetToken(rootToken)

	// Unseal and migrate to Transit.
	unsealMigrate(t, client, recoveryKeys, true)

	// Wait for migration to finish.  Sadly there is no callback, and the
	// test will fail later on if we don't do this.
	// TODO -- actually there is a callback, we can monitor this and await
	time.Sleep(10 * time.Second)

	// Read the secret
	secret, err := client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Make sure the seal configs were updated correctly.
	b, r, err := leader.Core.PhysicalSealConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verifyBarrierConfig(t, b, wrapping.Transit, 1, 1, 1)
	verifyBarrierConfig(t, r, wrapping.Shamir, keyShares, keyThreshold, 0)

	cluster.EnsureCoresSealed(t)

	return transitSeal
}

// TestSealMigration_ShamirToTransit_Post14 tests shamir-to-transit seal
// migration, using the post-1.4 method of bring individual nodes in the cluster
// to do the migration.
func TestSealMigration_ShamirToTransit_Post14(t *testing.T) {
	testVariousBackends(t, testSealMigrationShamirToTransit_Post14, true)
}

func testSealMigrationShamirToTransit_Post14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) {

	// Initialize the backend using shamir
	cluster, opts, cleanup := initializeShamir(t, logger, storage, basePort)
	//rootToken := cluster.RootToken

	// Create the transit server.
	tss := sealhelper.NewTransitSealServer(t)
	defer func() {
		tss.EnsureCoresSealed(t)
		tss.Cleanup()
	}()
	tss.MakeKey(t, "transit-seal-key")

	// Migrate the backend from shamir to transit.
	//transitSeal := migrateFromShamirToTransit_Post14(t, logger, storage, basePort, tss, cluster, opts)
	println("--------------------------------------------------------------------------------------------")
	_ = migrateFromShamirToTransit_Post14(t, logger, storage, basePort, tss, cluster, opts)
	cluster.EnsureCoresSealed(t)
	cleanup()
	println("--------------------------------------------------------------------------------------------")

	//// Run the backend with transit.
	//runTransit(t, logger, storage, basePort, rootToken, transitSeal)
}

func migrateFromShamirToTransit_Post14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	tss *sealhelper.TransitSealServer,
	cluster *vault.TestCluster, opts *vault.TestClusterOptions,
) vault.Seal {

	// N.B. Providing a transit seal puts us in migration mode.
	var transitSeal vault.Seal
	opts.SealFunc = func() vault.Seal {
		transitSeal = tss.MakeSeal(t, "transit-seal-key")
		return transitSeal
	}

	//var baseClusterPort = basePort + 10

	// Restart each follower with the new config, and migrate to Transit.
	// Note that the barrier keys are being used as recovery keys.
	rootToken, recoveryKeys := cluster.RootToken, cluster.BarrierKeys
	for i := 1; i < numTestCores; i++ {
		cluster.StopCore(t, i)
		if storage.IsRaft {
			teststorage.CloseRaftStorage(t, cluster.Cores[i])
		}
		cluster.RestartCore(t, i, opts)

		client := cluster.Cores[i].Client
		client.SetToken(rootToken)
		unsealMigrate(t, client, recoveryKeys, true)
		time.Sleep(5 * time.Second)
	}

	// Bring down the leader
	cluster.StopCore(t, 0)
	if storage.IsRaft {
		teststorage.CloseRaftStorage(t, cluster.Cores[0])
	}

	// Wait for the followers to establish a new leader
	leaderIdx, err := testhelpers.AwaitLeader(t, cluster)
	if err != nil {
		t.Fatal(err)
	}

	leader := cluster.Cores[leaderIdx]
	client := leader.Client
	client.SetToken(rootToken)

	// Wait for migration to finish.  Sadly there is no callback, and the
	// test will fail later on if we don't do this.
	// TODO -- actually there is a callback, we can monitor this and await
	time.Sleep(10 * time.Second)

	// Bring core 0 back up
	// TODO -- make sure its not migrating
	cluster.RestartCore(t, 0, opts)

	// TODO
	// if err := testhelpers.VerifyRaftConfiguration(leader, numTestCores); err != nil {

	// Read the secret
	secret, err := client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Make sure the seal configs were updated correctly.
	b, r, err := leader.Core.PhysicalSealConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verifyBarrierConfig(t, b, wrapping.Transit, 1, 1, 1)
	verifyBarrierConfig(t, r, wrapping.Shamir, keyShares, keyThreshold, 0)

	return transitSeal
}

func unsealMigrate(t *testing.T, client *api.Client, keys [][]byte, transitServerAvailable bool) {

	for i, key := range keys {

		// Try to unseal with missing "migrate" parameter
		_, err := client.Sys().UnsealWithOptions(&api.UnsealOpts{
			Key: base64.StdEncoding.EncodeToString(key),
		})
		if err == nil {
			t.Fatal("expected error due to lack of migrate parameter")
		}

		// Unseal with "migrate" parameter
		resp, err := client.Sys().UnsealWithOptions(&api.UnsealOpts{
			Key:     base64.StdEncoding.EncodeToString(key),
			Migrate: true,
		})

		if i < keyThreshold-1 {
			// Not enough keys have been provided yet.
			if err != nil {
				t.Fatal(err)
			}
		} else {
			if transitServerAvailable {
				// The transit server is running.
				if err != nil {
					t.Fatal(err)
				}
				if resp == nil || resp.Sealed {
					t.Fatalf("expected unsealed state; got %#v", resp)
				}
			} else {
				// The transit server is stopped.
				if err == nil {
					t.Fatal("expected error due to transit server being stopped.")
				}
			}
			break
		}
	}
}

// verifyBarrierConfig verifies that a barrier configuration is correct.
func verifyBarrierConfig(t *testing.T, cfg *vault.SealConfig, sealType string, shares, threshold, stored int) {
	t.Helper()
	if cfg.Type != sealType {
		t.Fatalf("bad seal config: %#v, expected type=%q", cfg, sealType)
	}
	if cfg.SecretShares != shares {
		t.Fatalf("bad seal config: %#v, expected SecretShares=%d", cfg, shares)
	}
	if cfg.SecretThreshold != threshold {
		t.Fatalf("bad seal config: %#v, expected SecretThreshold=%d", cfg, threshold)
	}
	if cfg.StoredShares != stored {
		t.Fatalf("bad seal config: %#v, expected StoredShares=%d", cfg, stored)
	}
}

// initializeShamir initializes a brand new backend storage with Shamir.
func initializeShamir(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) (*vault.TestCluster, *vault.TestClusterOptions, func()) {

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger: logger.Named("initializeShamir"),
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	cleanup := func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}

	leader := cluster.Cores[0]
	client := leader.Client

	// Unseal
	if storage.IsRaft {
		testhelpers.JoinRaftNodes(t, cluster, false,
			testhelpers.NewHardcodedServerAddressProvider(numTestCores, baseClusterPort))

		if err := testhelpers.VerifyRaftConfiguration(leader, numTestCores); err != nil {
			t.Fatal(err)
		}
	} else {
		cluster.UnsealCores(t)
	}
	testhelpers.WaitForNCoresUnsealed(t, cluster, numTestCores)

	// Write a secret that we will read back out later.
	_, err := client.Logical().Write(
		"secret/foo",
		map[string]interface{}{"zork": "quux"})
	if err != nil {
		t.Fatal(err)
	}

	return cluster, &opts, cleanup
}

// runShamir uses a pre-populated backend storage with Shamir.
func runShamir(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	rootToken string, barrierKeys [][]byte) {

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger: logger.Named("runShamir"),
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
		SkipInit:              true,
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	defer func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}()

	leader := cluster.Cores[0]
	client := leader.Client
	client.SetToken(rootToken)

	// Unseal
	cluster.BarrierKeys = barrierKeys
	if storage.IsRaft {
		provider := testhelpers.NewHardcodedServerAddressProvider(numTestCores, baseClusterPort)
		testhelpers.SetRaftAddressProviders(t, cluster, provider)

		for _, core := range cluster.Cores {
			cluster.UnsealCore(t, core)
		}

		// This is apparently necessary for the raft cluster to get itself
		// situated.
		time.Sleep(15 * time.Second)

		if err := testhelpers.VerifyRaftConfiguration(leader, numTestCores); err != nil {
			t.Fatal(err)
		}
	} else {
		cluster.UnsealCores(t)
	}
	testhelpers.WaitForNCoresUnsealed(t, cluster, numTestCores)

	// Read the secret
	secret, err := client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Seal the cluster
	cluster.EnsureCoresSealed(t)
}

// initializeTransit initializes a brand new backend storage with Transit.
func initializeTransit(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	tss *sealhelper.TransitSealServer) (string, [][]byte, vault.Seal) {

	var transitSeal vault.Seal

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger: logger.Named("initializeTransit"),
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
		SealFunc: func() vault.Seal {
			transitSeal = tss.MakeSeal(t, "transit-seal-key")
			return transitSeal
		},
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	defer func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}()

	leader := cluster.Cores[0]
	client := leader.Client

	// Join raft
	if storage.IsRaft {
		testhelpers.JoinRaftNodes(t, cluster, true,
			testhelpers.NewHardcodedServerAddressProvider(numTestCores, baseClusterPort))

		if err := testhelpers.VerifyRaftConfiguration(leader, numTestCores); err != nil {
			t.Fatal(err)
		}
	}
	testhelpers.WaitForNCoresUnsealed(t, cluster, numTestCores)

	// Write a secret that we will read back out later.
	_, err := client.Logical().Write(
		"secret/foo",
		map[string]interface{}{"zork": "quux"})
	if err != nil {
		t.Fatal(err)
	}

	// Seal the cluster
	cluster.EnsureCoresSealed(t)

	return cluster.RootToken, cluster.RecoveryKeys, transitSeal
}

func runTransit(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	rootToken string, transitSeal vault.Seal) {

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger: logger.Named("runTransit"),
		Seal:   transitSeal,
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
		SkipInit:              true,
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	defer func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}()

	leader := cluster.Cores[0]
	client := leader.Client
	client.SetToken(rootToken)

	// Unseal.  Even though we are using autounseal, we have to unseal
	// explicitly because we are using SkipInit.
	if storage.IsRaft {
		provider := testhelpers.NewHardcodedServerAddressProvider(numTestCores, baseClusterPort)
		testhelpers.SetRaftAddressProviders(t, cluster, provider)

		for _, core := range cluster.Cores {
			cluster.UnsealCoreWithStoredKeys(t, core)
		}

		// This is apparently necessary for the raft cluster to get itself
		// situated.
		time.Sleep(15 * time.Second)

		if err := testhelpers.VerifyRaftConfiguration(leader, numTestCores); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := cluster.UnsealCoresWithError(true); err != nil {
			t.Fatal(err)
		}
	}
	testhelpers.WaitForNCoresUnsealed(t, cluster, numTestCores)

	// Read the secret
	secret, err := client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Seal the cluster
	cluster.EnsureCoresSealed(t)
}

//--------------------------------------------------------------

func TestShamir(t *testing.T) {
	testVariousBackends(t, testShamir, true)
}

func testShamir(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) {

	// Initialize the backend using shamir
	cluster, _, cleanup := initializeShamir(t, logger, storage, basePort)
	rootToken, barrierKeys := cluster.RootToken, cluster.BarrierKeys
	cluster.EnsureCoresSealed(t)
	cleanup()

	runShamir(t, logger, storage, basePort, rootToken, barrierKeys)
}

func TestTransit(t *testing.T) {
	testVariousBackends(t, testTransit, true)
}

func testTransit(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) {

	tss := sealhelper.NewTransitSealServer(t)
	defer tss.Cleanup()
	tss.MakeKey(t, "transit-seal-key")

	rootToken, _, transitSeal := initializeTransit(t, logger, storage, basePort, tss)

	runTransit(t, logger, storage, basePort, rootToken, transitSeal)
}
