package simulation

import (
	"math/big"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obscuronet/obscuro-playground/integration"

	"github.com/obscuronet/obscuro-playground/go/obscuronode/wallet"

	"github.com/obscuronet/obscuro-playground/go/ethclient/erc20contractlib"

	"github.com/obscuronet/obscuro-playground/go/ethclient/mgmtcontractlib"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/obscuronet/obscuro-playground/go/ethclient"
	"github.com/obscuronet/obscuro-playground/go/log"
	"github.com/obscuronet/obscuro-playground/go/obscurocommon"
	"github.com/obscuronet/obscuro-playground/go/obscuronode/enclave/core"
	"github.com/obscuronet/obscuro-playground/go/obscuronode/nodecommon"
	"github.com/obscuronet/obscuro-playground/go/obscuronode/obscuroclient"
	"golang.org/x/sync/errgroup"

	stats2 "github.com/obscuronet/obscuro-playground/integration/simulation/stats"
	wallet_mock "github.com/obscuronet/obscuro-playground/integration/walletmock"
)

// TransactionInjector is a structure that generates, issues and tracks transactions
type TransactionInjector struct {
	// settings
	avgBlockDuration time.Duration
	stats            *stats2.Stats

	obsWallets []wallet_mock.Wallet
	ethWallets []wallet.Wallet

	l1Nodes       []ethclient.EthClient
	l2NodeClients []*obscuroclient.Client

	l1TransactionsLock sync.RWMutex
	l1Transactions     []obscurocommon.L1Transaction

	l2TransactionsLock sync.RWMutex
	l2Transactions     core.L2Txs

	interruptRun     *int32
	fullyStoppedChan chan bool

	erc20ContractAddr *common.Address
	mgmtContractAddr  *common.Address
	mgmtContractLib   mgmtcontractlib.MgmtContractLib
	erc20ContractLib  erc20contractlib.ERC20ContractLib
}

// NewTransactionInjector returns a transaction manager with a given number of obsWallets
// todo Add methods that generate deterministic scenarios
func NewTransactionInjector(
	avgBlockDuration time.Duration,
	stats *stats2.Stats,
	l1Nodes []ethclient.EthClient,
	ethWallets []wallet.Wallet,
	mgmtContractAddr *common.Address,
	erc20ContractAddr *common.Address,
	l2NodeClients []*obscuroclient.Client,
	mgmtContractLib mgmtcontractlib.MgmtContractLib,
	erc20ContractLib erc20contractlib.ERC20ContractLib,
) *TransactionInjector {
	// convert the eth wallets into obs wallets
	wallets := make([]wallet_mock.Wallet, len(ethWallets))
	for i := 0; i < len(ethWallets); i++ {
		wallets[i] = wallet_mock.NewFromPk(ethWallets[i].PK())
	}
	interrupt := int32(0)

	return &TransactionInjector{
		obsWallets:        wallets,
		avgBlockDuration:  avgBlockDuration,
		stats:             stats,
		l1Nodes:           l1Nodes,
		l2NodeClients:     l2NodeClients,
		interruptRun:      &interrupt,
		fullyStoppedChan:  make(chan bool),
		erc20ContractAddr: erc20ContractAddr,
		mgmtContractAddr:  mgmtContractAddr,
		mgmtContractLib:   mgmtContractLib,
		erc20ContractLib:  erc20ContractLib,
		ethWallets:        ethWallets,
	}
}

// Start begins the execution on the TransactionInjector
// Deposits an initial balance in to each wallet
// Generates and issues L1 and L2 transactions to the network
func (m *TransactionInjector) Start() {
	// deposit some initial amount into every user
	for _, w := range m.ethWallets {
		addr := w.Address()
		txData := &obscurocommon.L1DepositTx{
			Amount:        initialBalance,
			To:            m.mgmtContractAddr,
			TokenContract: m.erc20ContractAddr,
			Sender:        &addr,
		}
		tx := m.erc20ContractLib.CreateDepositTx(txData, w.GetNonceAndIncrement())
		signedTx, err := w.SignTransaction(tx)
		if err != nil {
			panic(err)
		}
		err = m.rndL1Node().SendTransaction(signedTx)
		if err != nil {
			panic(err)
		}

		m.stats.Deposit(initialBalance)
		go m.trackL1Tx(txData)
		time.Sleep(m.avgBlockDuration / 3)
	}

	// start transactions issuance
	var wg errgroup.Group
	wg.Go(func() error {
		m.issueRandomDeposits()
		return nil
	})

	wg.Go(func() error {
		m.issueRandomWithdrawals()
		return nil
	})

	wg.Go(func() error {
		m.issueRandomTransfers()
		return nil
	})

	wg.Go(func() error {
		m.issueInvalidWithdrawals()
		return nil
	})

	_ = wg.Wait() // future proofing to return errors
	m.fullyStoppedChan <- true
}

func (m *TransactionInjector) Stop() {
	atomic.StoreInt32(m.interruptRun, 1)
	for range m.fullyStoppedChan {
		log.Info("TransactionInjector stopped successfully")
		return
	}
}

// trackL1Tx adds an L1Tx to the internal list
func (m *TransactionInjector) trackL1Tx(tx obscurocommon.L1Transaction) {
	m.l1TransactionsLock.Lock()
	defer m.l1TransactionsLock.Unlock()
	m.l1Transactions = append(m.l1Transactions, tx)
}

// trackL2Tx adds an L2Tx to the internal list
func (m *TransactionInjector) trackL2Tx(tx nodecommon.L2Tx) {
	m.l2TransactionsLock.Lock()
	defer m.l2TransactionsLock.Unlock()
	m.l2Transactions = append(m.l2Transactions, tx)
}

// GetL1Transactions returns all generated L1 L2Txs
func (m *TransactionInjector) GetL1Transactions() []obscurocommon.L1Transaction {
	return m.l1Transactions
}

// GetL2Transactions returns all generated non-WithdrawalTx transactions
func (m *TransactionInjector) GetL2Transactions() (core.L2Txs, core.L2Txs) {
	var transfers, withdrawals core.L2Txs
	for _, req := range m.l2Transactions {
		r := req
		switch core.TxData(&r).Type {
		case core.TransferTx:
			transfers = append(transfers, req)
		case core.WithdrawalTx:
			withdrawals = append(withdrawals, req)
		case core.DepositTx:
		}
	}
	return transfers, withdrawals
}

// GetL2WithdrawalRequests returns generated stored WithdrawalTx transactions
func (m *TransactionInjector) GetL2WithdrawalRequests() []nodecommon.Withdrawal {
	var withdrawals []nodecommon.Withdrawal
	for _, req := range m.l2Transactions {
		tx := core.TxData(&req) //nolint:gosec
		if tx.Type == core.WithdrawalTx {
			withdrawals = append(withdrawals, nodecommon.Withdrawal{Amount: tx.Amount, Address: tx.To})
		}
	}
	return withdrawals
}

// issueRandomTransfers creates and issues a number of L2 transfer transactions proportional to the simulation time, such that they can be processed
func (m *TransactionInjector) issueRandomTransfers() {
	for ; atomic.LoadInt32(m.interruptRun) == 0; time.Sleep(obscurocommon.RndBtwTime(m.avgBlockDuration/4, m.avgBlockDuration)) {
		fromWallet := m.rndObsWallet()
		toWallet := m.rndObsWallet()
		for fromWallet.Address == toWallet.Address {
			toWallet = m.rndObsWallet()
		}
		tx := wallet_mock.NewL2Transfer(fromWallet.Address, toWallet.Address, obscurocommon.RndBtw(1, 500))
		signedTx := wallet_mock.SignTx(tx, fromWallet.Key.PrivateKey)
		encryptedTx := core.EncryptTx(signedTx)
		m.stats.Transfer()

		err := (*m.rndL2NodeClient()).Call(nil, obscuroclient.RPCSendTransactionEncrypted, encryptedTx)
		if err != nil {
			log.Info("Failed to issue transfer via RPC.")
			continue
		}

		go m.trackL2Tx(*signedTx)
	}
}

// issueRandomDeposits creates and issues a number of transactions proportional to the simulation time, such that they can be processed
func (m *TransactionInjector) issueRandomDeposits() {
	for ; atomic.LoadInt32(m.interruptRun) == 0; time.Sleep(obscurocommon.RndBtwTime(m.avgBlockDuration, m.avgBlockDuration*2)) {
		v := obscurocommon.RndBtw(1, 100)
		ethWallet := m.rndEthWallet()
		addr := ethWallet.Address()
		txData := &obscurocommon.L1DepositTx{
			Amount:        v,
			To:            m.mgmtContractAddr,
			TokenContract: m.erc20ContractAddr,
			Sender:        &addr,
		}
		tx := m.erc20ContractLib.CreateDepositTx(txData, ethWallet.GetNonceAndIncrement())
		signedTx, err := ethWallet.SignTransaction(tx)
		if err != nil {
			panic(err)
		}
		err = m.rndL1Node().SendTransaction(signedTx)
		if err != nil {
			panic(err)
		}

		m.stats.Deposit(v)
		go m.trackL1Tx(txData)
	}
}

// issueRandomWithdrawals creates and issues a number of transactions proportional to the simulation time, such that they can be processed
// Generates L2 enclave2.WithdrawalTx transactions
func (m *TransactionInjector) issueRandomWithdrawals() {
	for ; atomic.LoadInt32(m.interruptRun) == 0; time.Sleep(obscurocommon.RndBtwTime(m.avgBlockDuration, m.avgBlockDuration*2)) {
		v := obscurocommon.RndBtw(1, 100)
		obsWallet := m.rndObsWallet()
		tx := wallet_mock.NewL2Withdrawal(obsWallet.Address, v)
		signedTx := wallet_mock.SignTx(tx, obsWallet.Key.PrivateKey)
		encryptedTx := core.EncryptTx(signedTx)

		err := (*m.rndL2NodeClient()).Call(nil, obscuroclient.RPCSendTransactionEncrypted, encryptedTx)
		if err != nil {
			log.Info("Failed to issue withdrawal via RPC.")
			continue
		}

		m.stats.Withdrawal(v)
		go m.trackL2Tx(*signedTx)
	}
}

// issueInvalidWithdrawals creates and issues a number of invalidly-signed L2 withdrawal transactions proportional to the simulation time.
// These transactions should be rejected by the nodes, and thus we expect them not to show up in the simulation withdrawal checks.
func (m *TransactionInjector) issueInvalidWithdrawals() {
	for ; atomic.LoadInt32(m.interruptRun) == 0; time.Sleep(obscurocommon.RndBtwTime(m.avgBlockDuration/4, m.avgBlockDuration)) {
		fromWallet := m.rndObsWallet()
		tx := wallet_mock.NewL2Withdrawal(fromWallet.Address, obscurocommon.RndBtw(1, 100))
		signedTx := createInvalidSignature(tx, &fromWallet)
		encryptedTx := core.EncryptTx(signedTx)

		err := (*m.rndL2NodeClient()).Call(nil, obscuroclient.RPCSendTransactionEncrypted, encryptedTx)
		if err != nil {
			log.Info("Failed to issue withdrawal via RPC.")
			continue
		}
	}
}

// Uses one of three approaches to create an invalidly-signed transaction.
func createInvalidSignature(tx *nodecommon.L2Tx, fromWallet *wallet_mock.Wallet) *nodecommon.L2Tx {
	i := rand.Intn(3) //nolint:gosec
	switch i {
	case 0: // We sign the transaction with a bad signer.
		incorrectChainID := int64(integration.ChainID + 1)
		signer := types.NewLondonSigner(big.NewInt(incorrectChainID))
		signedTx, _ := types.SignTx(tx, signer, fromWallet.Key.PrivateKey)
		return signedTx

	case 1: // We do not sign the transaction.
		return tx

	case 2: // We modify the transaction after signing.
		// We create a new transaction, as we need access to the transaction's encapsulated transaction data.
		txData := core.L2TxData{Type: core.WithdrawalTx, From: fromWallet.Address, Amount: obscurocommon.RndBtw(1, 100)}
		newTx := wallet_mock.NewL2Tx(txData)
		wallet_mock.SignTx(newTx, fromWallet.Key.PrivateKey)
		// After signing the transaction, we create a new transaction based on the transaction data, breaking the signature.
		return wallet_mock.NewL2Tx(txData)
	}
	panic("Expected i to be in the range [0,2).")
}

func (m *TransactionInjector) rndObsWallet() wallet_mock.Wallet {
	return m.obsWallets[rand.Intn(len(m.obsWallets)-1)] //nolint:gosec
}

func (m *TransactionInjector) rndEthWallet() wallet.Wallet {
	return m.ethWallets[rand.Intn(len(m.ethWallets)-1)] //nolint:gosec
}

func (m *TransactionInjector) rndL1Node() ethclient.EthClient {
	return m.l1Nodes[rand.Intn(len(m.l1Nodes))] //nolint:gosec
}

func (m *TransactionInjector) rndL2NodeClient() *obscuroclient.Client {
	return m.l2NodeClients[rand.Intn(len(m.l2NodeClients))] //nolint:gosec
}
