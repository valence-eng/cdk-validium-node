package e2e

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/db"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/test/dbutils"
	"github.com/0xPolygonHermez/zkevm-node/test/operations"
	"github.com/0xPolygonHermez/zkevm-node/test/testutils"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPermissionlessJRPC(t *testing.T) {
	// Initial setup:
	// - permissionless RPC + Sync
	// - trusted node with everything minus EthTxMan (to prevent the trusted state from being virtualized)
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	defer func() { require.NoError(t, operations.TeardownPermissionless()) }()
	err := operations.Teardown()
	require.NoError(t, err)
	opsCfg := operations.GetDefaultOperationsConfig()
	opsCfg.State.MaxCumulativeGasUsed = 80000000000
	opsman, err := operations.NewManager(ctx, opsCfg)
	require.NoError(t, err)
	require.NoError(t, opsman.SetupWithPermissionless())
	require.NoError(t, opsman.StopEthTxSender())
	time.Sleep(5 * time.Second)

	// Step 1:
	// - actions: send nTxsStep1 transactions to the trusted sequencer through the permissionless sequencer
	// first transaction gets the current nonce. The others are generated
	// - assert: transactions are properly relayed, added in to the trusted state and broadcasted to the permissionless node

	nTxsStep1 := 10
	// Load account with balance on local genesis
	auth, err := operations.GetAuth(operations.DefaultSequencerPrivateKey, operations.DefaultL2ChainID)
	require.NoError(t, err)
	// Load eth client (permissionless RPC)
	client, err := ethclient.Dial(operations.PermissionlessL2NetworkURL)
	require.NoError(t, err)
	// Send txs
	amount := big.NewInt(10000)
	toAddress := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	senderBalance, err := client.BalanceAt(ctx, auth.From, nil)
	require.NoError(t, err)
	nonceToBeUsedForNextTx, err := client.PendingNonceAt(ctx, auth.From)
	require.NoError(t, err)

	log.Infof("Receiver Addr: %v", toAddress.String())
	log.Infof("Sender Addr: %v", auth.From.String())
	log.Infof("Sender Balance: %v", senderBalance.String())
	log.Infof("Sender Nonce: %v", nonceToBeUsedForNextTx)

	gasLimit, err := client.EstimateGas(ctx, ethereum.CallMsg{From: auth.From, To: &toAddress, Value: amount})
	require.NoError(t, err)

	gasPrice, err := client.SuggestGasPrice(ctx)
	require.NoError(t, err)

	txsStep1 := make([]*types.Transaction, 0, nTxsStep1)
	for i := 0; i < nTxsStep1; i++ {
		tx := types.NewTransaction(nonceToBeUsedForNextTx, toAddress, amount, gasLimit, gasPrice, nil)
		txsStep1 = append(txsStep1, tx)
		nonceToBeUsedForNextTx += 1
	}
	log.Infof("sending %d txs and waiting until added in the permissionless RPC trusted state")
	l2BlockNumbersStep1, err := operations.ApplyL2Txs(ctx, txsStep1, auth, client, operations.TrustedConfirmationLevel)
	require.NoError(t, err)

	// Step 2
	// - actions: stop the sequencer and send nTxsStep2 transactions, then use the getPendingNonce, and send tx with the resulting nonce
	// - assert: pendingNonce works as expected (force a scenario where the pool needs to be taken into consideration)
	nTxsStep2 := 10
	require.NoError(t, opsman.StopSequencer())
	require.NoError(t, opsman.StopSequenceSender())
	txsStep2 := make([]*types.Transaction, 0, nTxsStep2)
	for i := 0; i < nTxsStep2; i++ {
		tx := types.NewTransaction(nonceToBeUsedForNextTx, toAddress, amount, gasLimit, gasPrice, nil)
		txsStep2 = append(txsStep2, tx)
		nonceToBeUsedForNextTx += 1
	}
	log.Infof("sending %d txs and waiting until added into the trusted sequencer pool")
	_, err = operations.ApplyL2Txs(ctx, txsStep2, auth, client, operations.PoolConfirmationLevel)
	require.NoError(t, err)
	actualNonce, err := client.PendingNonceAt(ctx, auth.From)
	require.NoError(t, err)
	require.Equal(t, nonceToBeUsedForNextTx, actualNonce)
	// Step 3
	// - actions: start Sequencer and EthTxSender
	// - assert: all transactions get virtualized WITHOUT L2 reorgs
	require.NoError(t, opsman.StartSequencer())
	require.NoError(t, opsman.StartEthTxSender())
	require.NoError(t, opsman.StartSequenceSender())

	lastL2BlockNumberStep1 := l2BlockNumbersStep1[len(l2BlockNumbersStep1)-1]
	lastL2BlockNumberStep2 := lastL2BlockNumberStep1.Add(
		lastL2BlockNumberStep1,
		big.NewInt(int64(nTxsStep2)),
	)
	err = operations.WaitL2BlockToBeVirtualizedCustomRPC(
		lastL2BlockNumberStep2, 4*time.Minute, //nolint:gomnd
		operations.PermissionlessL2NetworkURL,
	)
	require.NoError(t, err)
	sqlDBPless, err := db.NewSQLDB(db.Config{
		User:      testutils.GetEnv("PERMISSIONLESSPGUSER", "test_user"),
		Password:  testutils.GetEnv("PERMISSIONLESSPGPASSWORD", "test_password"),
		Name:      testutils.GetEnv("PERMISSIONLESSPGDATABASE", "state_db"),
		Host:      testutils.GetEnv("PERMISSIONLESSPGHOST", "localhost"),
		Port:      testutils.GetEnv("PERMISSIONLESSPGPORT", "5434"),
		EnableLog: true,
		MaxConns:  4,
	})
	require.NoError(t, err)
	sqlDBTrusted, err := db.NewSQLDB(dbutils.NewStateConfigFromEnv())
	require.NoError(t, err)
	const isThereL2ReorgQuery = "SELECT COUNT(*) > 0 FROM state.trusted_reorg;"
	row := sqlDBPless.QueryRow(context.Background(), isThereL2ReorgQuery)
	isThereL2Reorg := true
	require.NoError(t, row.Scan(&isThereL2Reorg))
	require.False(t, isThereL2Reorg)

	// Assert that he permissionless node is fully synced
	clientTrusted, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	blockNum, err := clientTrusted.BlockNumber(ctx)
	require.NoError(t, err)
	blockNumBig := big.NewInt(int64(blockNum))
	// Wait for permissionless to be synced
	err = errors.New("wait for it")
	for err != nil {
		_, err = client.BlockByNumber(ctx, blockNumBig)
		time.Sleep(10 * time.Second)
		if err != nil {
			blockNumLess, _err := clientTrusted.BlockNumber(ctx)
			require.NoError(t, _err)
			log.Infof("trustless is at block %d, waiting to reach %d", blockNumLess, blockNum)
		}
	}
	const timestampQuery = "select floor(extract(epoch from timestamp))::integer from state.batch b inner join state.l2block l on b.batch_num = l.batch_num where l.block_num = $1;"
	for i := 1; i <= int(blockNum); i++ {
		blockNumBig = big.NewInt(int64(i))
		expectedBlock, err := clientTrusted.BlockByNumber(ctx, blockNumBig)
		require.NoError(t, err)
		actualBlock, err := client.BlockByNumber(ctx, blockNumBig)
		require.NoError(t, err)
		atBlockStr := fmt.Sprintf("missmatch at L2 block %d", i)
		assert.Equal(
			t, expectedBlock.Header().Time, actualBlock.Header().Time,
			atBlockStr,
		)

		row := sqlDBPless.QueryRow(context.Background(), timestampQuery, i)
		var timestampFromPlessDB uint64
		require.NoError(t, row.Scan(&timestampFromPlessDB))
		row = sqlDBTrusted.QueryRow(context.Background(), timestampQuery, i)
		var timestampFromTrustedDB uint64
		require.NoError(t, row.Scan(&timestampFromTrustedDB))
		log.Infof(
			"Block %d. Timestamp trusted-RPC: %d, pless-RPC: %d, trusted-DB: %d, pless-DB: %d",
			i, expectedBlock.Header().Time, actualBlock.Header().Time, timestampFromTrustedDB, timestampFromPlessDB,
		)
		// assert.Equal(
		// 	t, expectedBlock.Header().ReceiptHash.Hex(), actualBlock.Header().ReceiptHash.Hex(),
		// 	atBlockStr,
		// )
		// je, err := expectedBlock.Header().MarshalJSON()
		// require.NoError(t, err)
		// log.Info(string(je))
		// ja, err := actualBlock.Header().MarshalJSON()
		// require.NoError(t, err)
		// log.Info(string(ja))
		// assert.Equal(t, string(je), string(ja))
	}
	log.Info("done")
}
