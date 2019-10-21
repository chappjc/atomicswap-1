package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go/xdr"

	"github.com/stellar/go/strkey"

	"github.com/stellar/go/clients/horizon"
	"github.com/stellar/go/clients/horizonclient"
	hprotocol "github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/txnbuild"
	"github.com/threefoldtech/atomicswap/cmd/stellaratomicswap/stellar"
	"github.com/threefoldtech/atomicswap/timings"

	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
)

const verify = true

const secretSize = 32

var (
	targetNetwork = network.PublicNetworkPassphrase
)
var (
	flagset       = flag.NewFlagSet("", flag.ExitOnError)
	testnetFlag   = flagset.Bool("testnet", false, "use testnet network")
	automatedFlag = flagset.Bool("automated", false, "Use automated/unattended version with json output")
)

// There are two directions that the atomic swap can be performed, as the
// initiator can be on either chain.  This tool only deals with creating the
// Stellar transactions for these swaps.  A second tool should be used for the
// transaction on the other chain.  Any chain can be used so long as it supports
// OP_SHA256 and OP_CHECKLOCKTIMEVERIFY.
//
// Example scenarios using bitcoin as the second chain:
//
// Scenerio 1:
//   cp1 initiates (dcr)
//   cp2 participates with cp1 H(S) (xlm)
//   cp1 redeems xlm revealing S
//     - must verify H(S) in contract is hash of known secret
//   cp2 redeems dcr with S
//
// Scenerio 2:
//   cp1 initiates (xlm)
//   cp2 participates with cp1 H(S) (dcr)
//   cp1 redeems dcr revealing S
//     - must verify H(S) in contract is hash of known secret
//   cp2 redeems xlm with S

func init() {
	flagset.Usage = func() {
		fmt.Println("Usage: stellaratomicswap [flags] cmd [cmd args]")
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("  initiate <initiator seed> <participant address> <amount>")
		fmt.Println("  participate <participant seed> <initiator address> <amount> <secret hash>")
		fmt.Println("  redeem <receiver seed> <holdingAccountAdress> <secret>")
		fmt.Println("  refund <refund transaction>")
		fmt.Println("  extractsecret <holdingAccountAdress> <secret hash>")
		fmt.Println("  auditcontract <holdingAccountAdress> < refund transaction>")
		fmt.Println()
		fmt.Println("Flags:")
		flagset.PrintDefaults()
	}
}

type command interface {
	runCommand(client horizonclient.ClientInterface) error
}

// offline commands don't require wallet RPC.
type offlineCommand interface {
	command
	runOfflineCommand() error
}

type initiateCmd struct {
	InitiatorKeyPair *keypair.Full
	cp2Addr          string
	amount           string
}

type participateCmd struct {
	cp1Addr             string
	participatorKeyPair *keypair.Full
	amount              string
	secretHash          []byte
}

type redeemCmd struct {
	ReceiverKeyPair       *keypair.Full
	holdingAccountAddress string
	secret                []byte
}

type refundCmd struct {
	refundTx txnbuild.Transaction
}

type extractSecretCmd struct {
	holdingAccountAdress string
	secretHash           string
}

type auditContractCmd struct {
	refundTx             txnbuild.Transaction
	holdingAccountAdress string
}

func main() {
	showUsage, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if showUsage {
		flagset.Usage()
	}
	if err != nil || showUsage {
		os.Exit(1)
	}
}

func checkCmdArgLength(args []string, required int) (nArgs int) {
	if len(args) < required {
		return 0
	}
	for i, arg := range args[:required] {
		if len(arg) != 1 && strings.HasPrefix(arg, "-") {
			return i
		}
	}
	return required
}
func run() (showUsage bool, err error) {
	flagset.Parse(os.Args[1:])
	args := flagset.Args()
	if len(args) == 0 {
		return true, nil
	}
	cmdArgs := 0
	switch args[0] {
	case "initiate":
		cmdArgs = 3
	case "participate":
		cmdArgs = 4
	case "redeem":
		cmdArgs = 3
	case "refund":
		cmdArgs = 1
	case "extractsecret":
		cmdArgs = 2
	case "auditcontract":
		cmdArgs = 2
	default:
		return true, fmt.Errorf("unknown command %v", args[0])
	}
	nArgs := checkCmdArgLength(args[1:], cmdArgs)
	flagset.Parse(args[1+nArgs:])
	if nArgs < cmdArgs {
		return true, fmt.Errorf("%s: too few arguments", args[0])
	}
	if flagset.NArg() != 0 {
		return true, fmt.Errorf("unexpected argument: %s", flagset.Arg(0))
	}

	if *testnetFlag {
		targetNetwork = network.TestNetworkPassphrase
	}

	var client horizonclient.ClientInterface
	switch targetNetwork {
	case network.PublicNetworkPassphrase:
		client = horizonclient.DefaultPublicNetClient
	case network.TestNetworkPassphrase:
		client = horizonclient.DefaultTestNetClient

	}

	var cmd command
	switch args[0] {
	case "initiate":
		initiatorKeypair, err := keypair.Parse(args[1])
		if err != nil {
			return true, fmt.Errorf("invalid initiator seed: %v", err)
		}
		initiatorFullKeypair, ok := initiatorKeypair.(*keypair.Full)
		if !ok {
			return true, errors.New("invalid initiator seed")
		}

		_, err = keypair.Parse(args[2])
		if err != nil {
			return true, fmt.Errorf("invalid participant address: %v", err)
		}

		_, err = strconv.ParseFloat(args[3], 64)
		if err != nil {
			return true, fmt.Errorf("failed to decode amount: %v", err)
		}

		cmd = &initiateCmd{InitiatorKeyPair: initiatorFullKeypair, cp2Addr: args[2], amount: args[3]}
	case "participate":
		participatorKeypair, err := keypair.Parse(args[1])
		if err != nil {
			return true, fmt.Errorf("invalid participator seed: %v", err)
		}
		participatorFullKeypair, ok := participatorKeypair.(*keypair.Full)
		if !ok {
			return true, errors.New("invalid participator seed")
		}

		_, err = keypair.Parse(args[2])
		if err != nil {
			return true, fmt.Errorf("invalid initiator address: %v", err)
		}

		_, err = strconv.ParseFloat(args[3], 64)
		if err != nil {
			return true, fmt.Errorf("failed to decode amount: %v", err)
		}

		secretHash, err := hex.DecodeString(args[4])
		if err != nil {
			return true, errors.New("secret hash must be hex encoded")
		}
		if len(secretHash) != sha256.Size {
			return true, errors.New("secret hash has wrong size")
		}
		cmd = &participateCmd{participatorKeyPair: participatorFullKeypair, cp1Addr: args[2], amount: args[3], secretHash: secretHash}
	case "auditcontract":
		_, err = keypair.Parse(args[1])
		if err != nil {
			return true, fmt.Errorf("invalid holding account address: %v", err)
		}
		refundTransaction, err := txnbuild.TransactionFromXDR(args[2])
		if err != nil {
			return true, fmt.Errorf("failed to decode refund transaction: %v", err)
		}
		cmd = &auditContractCmd{holdingAccountAdress: args[1], refundTx: refundTransaction}
	case "refund":

		refundTransaction, err := txnbuild.TransactionFromXDR(args[1])
		if err != nil {
			return true, fmt.Errorf("failed to decode refund transaction: %v", err)
		}
		cmd = &refundCmd{refundTx: refundTransaction}
	case "redeem":

		receiverKeypair, err := keypair.Parse(args[1])
		if err != nil {
			return true, fmt.Errorf("invalid receiver seed: %v", err)
		}
		receiverFullKeypair, ok := receiverKeypair.(*keypair.Full)
		if !ok {
			return true, errors.New("invalid receiver seed")
		}
		_, err = keypair.Parse(args[2])
		if err != nil {
			return true, fmt.Errorf("invalid holding account address: %v", err)
		}
		secret, err := hex.DecodeString(args[3])
		if err != nil {
			return true, fmt.Errorf("failed to decode secret: %v", err)
		}
		if len(secret) != secretSize {
			return true, fmt.Errorf("The secret should be %d bytes instead of %d", secretSize, len(secret))
		}
		cmd = &redeemCmd{ReceiverKeyPair: receiverFullKeypair, holdingAccountAddress: args[2], secret: secret}

	case "extractsecret":

		_, err = keypair.Parse(args[1])
		if err != nil {
			return true, fmt.Errorf("invalid holding account address: %v", err)
		}
		cmd = &extractSecretCmd{holdingAccountAdress: args[1], secretHash: args[2]}
	}
	err = cmd.runCommand(client)
	return false, err
}

func sha256Hash(x []byte) []byte {
	h := sha256.Sum256(x)
	return h[:]
}
func createRefundTransaction(holdingAccountAddress string, refundAccountAdress string, locktime time.Time, client horizonclient.ClientInterface) (refundTransaction txnbuild.Transaction, err error) {
	holdingAccount, err := stellar.GetAccount(holdingAccountAddress, client)
	if err != nil {
		return
	}
	_, err = holdingAccount.IncrementSequenceNumber()
	if err != nil {
		return
	}

	mergeAccountOperation := txnbuild.AccountMerge{
		Destination:   refundAccountAdress,
		SourceAccount: holdingAccount,
	}
	refundTransaction = txnbuild.Transaction{
		Timebounds: txnbuild.NewTimebounds(locktime.Unix(), int64(0)),
		Operations: []txnbuild.Operation{
			&mergeAccountOperation,
		},
		Network:       targetNetwork,
		SourceAccount: holdingAccount,
	}

	if err = refundTransaction.Build(); err != nil {
		err = fmt.Errorf("Failed to build the refund transaction: %s", err)
		return
	}
	return
}

//createHoldingAccountTransaction creates a new account to hold the atomic swap balance
//with the signers modified to the atomic swap rules:
//- signature of the destinee and the secret
//- hash of a specific transaction that is present on the chain
//    that merges the escrow account to the account that needs to withdraw
//    and that can only be published in the future ( timeout mechanism)
func createHoldingAccountTransaction(holdingAccountAddress string, xlmAmount string, fundingAccount *horizon.Account, network string) (createAccountTransaction txnbuild.Transaction, err error) {

	accountCreationOperation := txnbuild.CreateAccount{
		Destination:   holdingAccountAddress,
		Amount:        xlmAmount,
		SourceAccount: fundingAccount,
	}

	createAccountTransaction = txnbuild.Transaction{
		SourceAccount: fundingAccount,
		Operations: []txnbuild.Operation{
			&accountCreationOperation,
		},
		Network:    network,
		Timebounds: txnbuild.NewInfiniteTimeout(), //TODO: Use a real timeout
	}

	return
}

//createHoldingAccount creates a new account to hold the atomic swap balance
func createHoldingAccount(holdingAccountAddress string, xlmAmount string, fundingKeyPair *keypair.Full, network string, client horizonclient.ClientInterface) (err error) {

	fundingAccount, err := stellar.GetAccount(fundingKeyPair.Address(), client)
	if err != nil {
		return fmt.Errorf("Failed to get the funding account:%s", err)
	}
	createAccountTransaction, err := createHoldingAccountTransaction(holdingAccountAddress, xlmAmount, fundingAccount, network)
	if err != nil {
		return fmt.Errorf("Failed to create the holding account transaction: %s", err)
	}
	txe, err := createAccountTransaction.BuildSignEncode(fundingKeyPair)
	if err != nil {
		return fmt.Errorf("Failed to sign the holding account transaction: %s", err)
	}
	_, err = stellar.SubmitTransaction(txe, client)
	if err != nil {
		return fmt.Errorf("Failed to publish the holding account creation transaction : %s", err)
	}
	return
}
func createHoldingAccountSigningTransaction(holdingAccount *horizon.Account, counterPartyAddress string, secretHash []byte, refundTxHash []byte, network string) (setOptionsTransaction txnbuild.Transaction, err error) {

	depositorSigningOperation := txnbuild.SetOptions{
		Signer: &txnbuild.Signer{
			Address: counterPartyAddress,
			Weight:  1,
		},
		SourceAccount: holdingAccount,
	}
	secretHashAddress, err := stellar.CreateHashxAddress(secretHash)
	if err != nil {
		return
	}
	secretSigningOperation := txnbuild.SetOptions{
		Signer: &txnbuild.Signer{
			Address: secretHashAddress,
			Weight:  1,
		},
		SourceAccount: holdingAccount,
	}
	refundTxHashAdddress, err := stellar.CreateHashTxAddress(refundTxHash)
	if err != nil {
		return
	}
	refundSigningOperation := txnbuild.SetOptions{
		Signer: &txnbuild.Signer{
			Address: refundTxHashAdddress,
			Weight:  2,
		},
		SourceAccount: holdingAccount,
	}
	setSigingWeightsOperation := txnbuild.SetOptions{
		MasterWeight:    txnbuild.NewThreshold(txnbuild.Threshold(uint8(0))),
		LowThreshold:    txnbuild.NewThreshold(txnbuild.Threshold(2)),
		MediumThreshold: txnbuild.NewThreshold(txnbuild.Threshold(2)),
		HighThreshold:   txnbuild.NewThreshold(txnbuild.Threshold(2)),
		SourceAccount:   holdingAccount,
	}
	setOptionsTransaction = txnbuild.Transaction{
		SourceAccount: holdingAccount, //TODO: check if this can be changed to the fundingaccount
		Operations: []txnbuild.Operation{
			&depositorSigningOperation,
			&secretSigningOperation,
			&refundSigningOperation,
			&setSigingWeightsOperation,
		},
		Network:    network,
		Timebounds: txnbuild.NewInfiniteTimeout(), //TODO: Use a real timeout
	}

	return
}
func setHoldingAccountSigningOptions(holdingAccountKeyPair *keypair.Full, counterPartyAddress string, secretHash []byte, refundTxHash []byte, network string, client horizonclient.ClientInterface) (err error) {

	holdingAccountAddress := holdingAccountKeyPair.Address()
	holdingAccount, err := stellar.GetAccount(holdingAccountAddress, client)
	if err != nil {
		return fmt.Errorf("Failed to get the holding account: %s", err)
	}
	setSigningOptionsTransaction, err := createHoldingAccountSigningTransaction(holdingAccount, counterPartyAddress, secretHash, refundTxHash, targetNetwork)
	if err != nil {
		return fmt.Errorf("Failed to create the signing options transaction: %s", err)
	}
	txe, err := setSigningOptionsTransaction.BuildSignEncode(holdingAccountKeyPair)
	if err != nil {
		return fmt.Errorf("Failed to sign the signing options transaction: %s", err)
	}
	_, err = stellar.SubmitTransaction(txe, client)
	if err != nil {
		return fmt.Errorf("Failed to publish the signing options transaction : %s", err)
	}
	return
}
func createAtomicSwapHoldingAccount(fundingKeyPair *keypair.Full, holdingAccountKeyPair *keypair.Full, counterPartyAddress string, xlmAmount string, secretHash []byte, locktime time.Time, client horizonclient.ClientInterface) (refundTransaction txnbuild.Transaction, err error) {

	holdingAccountAddress := holdingAccountKeyPair.Address()
	err = createHoldingAccount(holdingAccountAddress, xlmAmount, fundingKeyPair, targetNetwork, client)
	if err != nil {
		return
	}

	refundTransaction, err = createRefundTransaction(holdingAccountAddress, fundingKeyPair.Address(), locktime, client)
	if err != nil {
		return
	}
	refundTransactionHash, err := refundTransaction.Hash()
	if err != nil {
		err = fmt.Errorf("Failed to Hash the refund transaction: %s", err)
		return
	}
	err = setHoldingAccountSigningOptions(holdingAccountKeyPair, counterPartyAddress, secretHash, refundTransactionHash[:], targetNetwork, client)

	return
}
func (cmd *initiateCmd) runCommand(client horizonclient.ClientInterface) error {
	var secret [secretSize]byte
	_, err := rand.Read(secret[:])
	if err != nil {
		return err
	}
	secretHash := sha256Hash(secret[:])
	fundingAccountAddress := cmd.InitiatorKeyPair.Address()
	holdingAccountKeyPair, err := stellar.GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("Failed to create holding account keypair: %s", err)
	}
	holdingAccountAddress := holdingAccountKeyPair.Address()
	//TODO: print the holding account private key in case of an error further down this function
	//to recover the funds

	locktime := time.Now().Add(timings.LockTime)
	refundTransaction, err := createAtomicSwapHoldingAccount(cmd.InitiatorKeyPair, holdingAccountKeyPair, cmd.cp2Addr, cmd.amount, secretHash, locktime, client)
	if err != nil {
		return err
	}

	serializedRefundTx, err := refundTransaction.Base64()
	if err != nil {
		return err
	}
	if !*automatedFlag {
		fmt.Printf("Secret:      %x\n", secret)
		fmt.Printf("Secret hash: %x\n\n", secretHash)
		fmt.Printf("initiator address: %s\n", fundingAccountAddress)
		fmt.Printf("holding account address: %s\n", holdingAccountAddress)
		fmt.Printf("refund transaction:\n%s\n", serializedRefundTx)
	} else {
		output := struct {
			Secret                string `json:"secret"`
			SecretHash            string `json:"hash"`
			InitiatorAddress      string `json:"initiator"`
			HoldingAccountAddress string `json:"holdingaccount"`
			RefundTransaction     string `json:"refundtransaction"`
		}{fmt.Sprintf("%x", secret),
			fmt.Sprintf("%x", secretHash),
			fundingAccountAddress,
			holdingAccountAddress,
			serializedRefundTx,
		}
		jsonoutput, _ := json.Marshal(output)
		fmt.Println(string(jsonoutput))
	}
	return nil
}

func (cmd *participateCmd) runCommand(client horizonclient.ClientInterface) error {

	fundingAccountAddress := cmd.participatorKeyPair.Address()
	holdingAccountKeyPair, err := stellar.GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("Failed to create holding account keypair: %s", err)
	}
	holdingAccountAddress := holdingAccountKeyPair.Address()
	//TODO: print the holding account private key in case of an error further down this function
	//to recover the funds

	locktime := time.Now().Add(timings.LockTime / 2)
	refundTransaction, err := createAtomicSwapHoldingAccount(cmd.participatorKeyPair, holdingAccountKeyPair, cmd.cp1Addr, cmd.amount, cmd.secretHash, locktime, client)
	if err != nil {
		return err
	}

	serializedRefundTx, err := refundTransaction.Base64()
	if err != nil {
		return err
	}
	if !*automatedFlag {
		fmt.Printf("participant address: %s\n", fundingAccountAddress)
		fmt.Printf("holding account address: %s\n", holdingAccountAddress)
		fmt.Printf("refund transaction:\n%s\n", serializedRefundTx)
	} else {

		output := struct {
			InitiatorAddress      string `json:"partcipant"`
			HoldingAccountAddress string `json:"holdingaccount"`
			RefundTransaction     string `json:"refundtransaction"`
		}{
			fundingAccountAddress,
			holdingAccountAddress,
			serializedRefundTx,
		}
		jsonoutput, _ := json.Marshal(output)
		fmt.Println(string(jsonoutput))
	}
	return nil
}

func (cmd *auditContractCmd) runCommand(client horizonclient.ClientInterface) error {
	holdingAccount, err := client.AccountDetail(horizonclient.AccountRequest{AccountID: cmd.holdingAccountAdress})
	if err != nil {
		return fmt.Errorf("Error getting the holding account details: %v", err)
	}
	balance, err := holdingAccount.GetNativeBalance() //TODO: modify for other assets
	if err != nil {
		return err
	}
	//Check if the signing tresholds are correct
	if holdingAccount.Thresholds.HighThreshold != 2 || holdingAccount.Thresholds.MedThreshold != 2 || holdingAccount.Thresholds.LowThreshold != 2 {
		return fmt.Errorf("Holding account signing tresholds are wrong.\nTresholds: High: %d, Medium: %d, Low: %d", holdingAccount.Thresholds.HighThreshold, holdingAccount.Thresholds.MedThreshold, holdingAccount.Thresholds.LowThreshold)
	}
	//Get the signing conditions
	var refundTxHashFromSigningConditions []byte
	recipientAddress := ""
	var secretHash []byte
	for _, signer := range holdingAccount.Signers {
		if signer.Weight == 0 { //The original keypair's signing weight is set to 0
			continue
		}
		switch signer.Type {
		case hprotocol.KeyTypeNames[strkey.VersionByteAccountID]:
			if recipientAddress != "" {
				return fmt.Errorf("Multiple recipients as signer: %s and %s", recipientAddress, signer.Key)
			}
			recipientAddress = signer.Key
			if signer.Weight != 1 {
				return fmt.Errorf("Signing weight of the recipient is wrong. Recipient: %s Weight: %d", signer.Key, signer.Weight)
			}
		case hprotocol.KeyTypeNames[strkey.VersionByteHashTx]:
			if refundTxHashFromSigningConditions != nil {
				return errors.New("Multiple refund transaction hashes as signer")
			}

			refundTxHashFromSigningConditions, err = strkey.Decode(strkey.VersionByteHashTx, signer.Key)
			if err != nil {
				return fmt.Errorf("Faulty encoded refund transaction hash: %s", err)
			}
			if signer.Weight != 2 {
				return fmt.Errorf("Signing weight of the refund transaction is wrong. Weight: %d", signer.Weight)
			}

		case hprotocol.KeyTypeNames[strkey.VersionByteHashX]:
			if secretHash != nil {
				return fmt.Errorf("Multiple secret hashes  transaction hashes as signer: %s and %s", secretHash, signer.Key)
			}
			secretHash, err = strkey.Decode(strkey.VersionByteHashX, signer.Key)
			if err != nil {
				return fmt.Errorf("Faulty encoded secret hash: %s", err)
			}
			if signer.Weight != 1 {
				return fmt.Errorf("Signing weight of the secret hash is wrong. Weight: %d", signer.Weight)
			}
		default:
			return fmt.Errorf("Unexpected signer type: %s", signer.Type)
		}
	}
	//Make sure all signing conditions are present
	if refundTxHashFromSigningConditions == nil {
		return errors.New("Missing refund transaction hash as signer")
	}
	if secretHash == nil {
		return errors.New("Missing secret as signer")
	}
	if recipientAddress == "" {
		return errors.New("Missing recipient as signer")
	}
	//Compare the refund transaction hash in the signing condition to the one of the passed refund transaction
	cmd.refundTx.Network = targetNetwork
	refundTxHash, err := cmd.refundTx.Hash()
	if err != nil {
		return fmt.Errorf("Unable to hash the passed refund transaction: %v", err)
	}
	if !bytes.Equal(refundTxHashFromSigningConditions, refundTxHash[:]) {
		return errors.New("Refund transaction hash in the signing condition is not equal to the one of the passed refund transaction")
	}
	//and finally get the locktime and refund address
	lockTime := cmd.refundTx.Timebounds.MinTime
	if len(cmd.refundTx.Operations) != 1 {
		return fmt.Errorf("Refund transaction is expected to have 1 operation instead of %d", len(cmd.refundTx.Operations))
	}
	refundoperation := cmd.refundTx.Operations[0]
	accountMergeOperation, ok := cmd.refundTx.Operations[0].(*txnbuild.AccountMerge)
	if !ok {
		return fmt.Errorf("Expecting an accountmerge operation in the refund transaction but got a %v", reflect.TypeOf(refundoperation))
	}
	if accountMergeOperation.SourceAccount.GetAccountID() != cmd.holdingAccountAdress {
		return fmt.Errorf("The refund transaction does not refund from the holding account but from %v", accountMergeOperation.SourceAccount.GetAccountID())
	}
	refundAddress := accountMergeOperation.Destination
	if !*automatedFlag {
		fmt.Printf("Contract address:        %v\n", cmd.holdingAccountAdress)
		fmt.Printf("Contract value:          %v\n", balance)
		fmt.Printf("Recipient address:       %v\n", recipientAddress)
		fmt.Printf("Refund address: %v\n\n", refundAddress)

		fmt.Printf("Secret hash: %x\n\n", secretHash)

		t := time.Unix(lockTime, 0)
		fmt.Printf("Locktime: %v\n", t.UTC())
		reachedAt := time.Until(t).Truncate(time.Second)
		if reachedAt > 0 {
			fmt.Printf("Locktime reached in %v\n", reachedAt)
		} else {
			fmt.Printf("Refund time lock has expired\n")
		}
	} else {
		output := struct {
			ContractAddress  string `json:"contractAddress"`
			ContractValue    string `json:"contractValue"`
			RecipientAddress string `json:"recipientAddress"`
			RefundAddress    string `json:"refundAddress"`
			SecretHash       string `json:"secretHash"`
			Locktime         string `json:"Locktime"`
		}{
			fmt.Sprintf("%v", cmd.holdingAccountAdress),
			fmt.Sprintf("%v", balance),
			recipientAddress,
			refundAddress,
			fmt.Sprintf("%x", secretHash),
			"",
		}
		t := time.Unix(lockTime, 0)
		output.Locktime = fmt.Sprintf("%v", t.UTC())
		jsonoutput, _ := json.Marshal(output)
		fmt.Println(string(jsonoutput))
	}
	return nil
}

func (cmd *refundCmd) runCommand(client horizonclient.ClientInterface) error {
	txe, err := cmd.refundTx.Base64()
	if err != nil {
		return err
	}
	result, err := stellar.SubmitTransaction(txe, client)
	if err != nil {
		return err
	}
	if !*automatedFlag {
		fmt.Println(result.TransactionSuccessToString())
	}
	return nil
}

func (cmd *redeemCmd) runCommand(client horizonclient.ClientInterface) error {
	holdingAccount, err := stellar.GetAccount(cmd.holdingAccountAddress, client)
	if err != nil {
		return err
	}
	receiverAddress := cmd.ReceiverKeyPair.Address()
	mergeAccountOperation := txnbuild.AccountMerge{
		Destination:   receiverAddress,
		SourceAccount: holdingAccount,
	}
	redeemTransaction := txnbuild.Transaction{
		Timebounds: txnbuild.NewTimebounds(int64(0), int64(0)),
		Operations: []txnbuild.Operation{
			&mergeAccountOperation,
		},
		Network:       targetNetwork,
		SourceAccount: holdingAccount,
	}

	err = redeemTransaction.Build()
	if err != nil {
		return fmt.Errorf("Unable to build the transaction: %v", err)
	}
	err = redeemTransaction.SignHashX(cmd.secret)
	if err != nil {
		return fmt.Errorf("Unable to sign with the secret:%v", err)
	}
	err = redeemTransaction.Sign(cmd.ReceiverKeyPair)
	if err != nil {
		return fmt.Errorf("Unable to sign with the receiver keypair:%v", err)
	}

	txe, err := redeemTransaction.Base64()
	if err != nil {
		return fmt.Errorf("Unable to uncode the transaction: %v", err)
	}

	txSuccess, err := stellar.SubmitTransaction(txe, client)
	if err != nil {
		return err
	}

	if !*automatedFlag {
		fmt.Println(txSuccess.TransactionSuccessToString())
	} else {
		output := struct {
			RedeemTransactionTxHash string `json:"redeemTransaction"`
		}{
			fmt.Sprintf("%v", txSuccess.Hash),
		}
		jsonoutput, _ := json.Marshal(output)
		fmt.Println(string(jsonoutput))
	}
	return nil
}

func (cmd *extractSecretCmd) runCommand(client horizonclient.ClientInterface) error {
	transactions, err := stellar.GetAccountDebitediTransactions(cmd.holdingAccountAdress, client)
	if err != nil {
		return fmt.Errorf("Error getting the transaction that debited the holdingAccount: %v", err)
	}
	switch len(transactions) {
	case 1:
	case 0:
		return errors.New("The holdingaccount has not been redeemed yet")
	default:
		return errors.New("Multiple spending transactions found") //TODO:find the good one
	}
	var extractedSecret []byte
	for _, rawSignature := range transactions[0].Signatures {

		decodedSignature, err := base64.StdEncoding.DecodeString(rawSignature)
		if err != nil {
			return fmt.Errorf("Error base64 decoding signature :%v", err)
		}
		if len(decodedSignature) > xdr.Signature(decodedSignature).XDRMaxSize() {
			continue // this is certainly not the secret we are looking for
		}
		signatureHash := sha256.Sum256(decodedSignature)
		hexSignatureHash := fmt.Sprintf("%x", signatureHash)
		if hexSignatureHash == cmd.secretHash {
			extractedSecret = decodedSignature
			break
		}
	}

	if extractedSecret == nil {
		return errors.New("Unable to find the matching secret")
	}
	fmt.Printf("Extracted secret: %x\n", extractedSecret)
	return nil
}