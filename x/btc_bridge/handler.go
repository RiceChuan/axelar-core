package btc_bridge

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/tendermint/tendermint/libs/log"

	"github.com/axelarnetwork/axelar-core/x/axelar/exported"
	"github.com/axelarnetwork/axelar-core/x/btc_bridge/keeper"
	"github.com/axelarnetwork/axelar-core/x/btc_bridge/types"
)

const bitcoin = "bitcoin"

func NewHandler(k keeper.Keeper, v types.Voter, rpc types.RPCClient, s types.Signer) sdk.Handler {
	return func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
		ctx = ctx.WithEventManager(sdk.NewEventManager())
		switch msg := msg.(type) {
		case types.MsgTrackAddress:
			return handleMsgTrackAddress(ctx, k, rpc, msg)
		case types.MsgTrackPubKey:
			return handleMsgTrackPubKey(ctx, k, rpc, s, msg)
		case types.MsgVerifyTx:
			return handleMsgVerifyTx(ctx, k, v, rpc, msg)
		case types.MsgRawTx:
			return handleMsgRawTx(ctx, k, v, msg)
		case types.MsgWithdraw:
			return handleMsgWithdraw(ctx, k, rpc, s, msg)
		default:
			return nil, sdkerrors.Wrap(sdkerrors.ErrUnknownRequest,
				fmt.Sprintf("unrecognized %s message type: %T", types.ModuleName, msg))
		}
	}
}

func handleMsgTrackAddress(ctx sdk.Context, k keeper.Keeper, rpc types.RPCClient, msg types.MsgTrackAddress) (*sdk.Result, error) {
	k.Logger(ctx).Debug(fmt.Sprintf("start tracking address %v", msg.Address))

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeModule),
			sdk.NewAttribute(sdk.AttributeKeySender, msg.Sender.String()),
			sdk.NewAttribute(types.AttributeAddress, msg.Address.String()),
		),
	)

	trackAddress(ctx, k, rpc, msg.Address.String(), msg.Rescan)

	return &sdk.Result{Events: ctx.EventManager().Events()}, nil
}

func handleMsgTrackPubKey(ctx sdk.Context, k keeper.Keeper, rpc types.RPCClient, s types.Signer, msg types.MsgTrackPubKey) (*sdk.Result, error) {
	key, err := s.GetKey(ctx, msg.KeyID)
	if err != nil {
		return nil, fmt.Errorf("keyId not recognized")
	}

	addr, err := addressFromKey(key, msg.Chain)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "could not convert the given public key into a bitcoin address")
	}

	trackAddress(ctx, k, rpc, addr.EncodeAddress(), false)

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeModule),
			sdk.NewAttribute(sdk.AttributeKeySender, msg.Sender.String()),
			sdk.NewAttribute(types.AttributeKeyId, msg.KeyID),
			sdk.NewAttribute(types.AttributeAddress, addr.EncodeAddress()),
		),
	)

	return &sdk.Result{Log: "successfully created a tracked address", Events: ctx.EventManager().Events()}, nil
}

func handleMsgVerifyTx(ctx sdk.Context, k keeper.Keeper, v types.Voter, rpc types.RPCClient, msg types.MsgVerifyTx) (*sdk.Result, error) {
	k.Logger(ctx).Debug("verifying bitcoin transaction")
	txId := msg.UTXO.Hash.String()
	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeModule),
			sdk.NewAttribute(sdk.AttributeKeySender, msg.Sender.String()),
			sdk.NewAttribute(types.AttributeTxId, txId),
			sdk.NewAttribute(types.AttributeAmount, msg.UTXO.Amount.String()),
		),
	)

	k.SetUTXO(ctx, txId, msg.UTXO)

	txForVote := exported.ExternalTx{Chain: bitcoin, TxID: txId}
	if err := verifyTx(rpc, msg.UTXO, k.GetConfirmationHeight(ctx)); err != nil {
		v.SetFutureVote(ctx, exported.FutureVote{Tx: txForVote, LocalAccept: false})

		k.Logger(ctx).Debug(sdkerrors.Wrapf(err,
			"expected transaction (%s) could not be verified", txId).Error())
		return &sdk.Result{
			Log:    err.Error(),
			Data:   k.Codec().MustMarshalBinaryLengthPrefixed(false),
			Events: ctx.EventManager().Events(),
		}, nil
	} else {
		v.SetFutureVote(ctx, exported.FutureVote{Tx: txForVote, LocalAccept: true})
		return &sdk.Result{
			Log:    "successfully verified transaction",
			Data:   k.Codec().MustMarshalBinaryLengthPrefixed(true),
			Events: ctx.EventManager().Events(),
		}, nil
	}
}

func handleMsgRawTx(ctx sdk.Context, k keeper.Keeper, v types.Voter, msg types.MsgRawTx) (*sdk.Result, error) {
	txId := msg.TxHash.String()
	if !v.IsVerified(ctx, exported.ExternalTx{
		Chain: bitcoin,
		TxID:  txId,
	}) {
		return nil, fmt.Errorf("transaction not verified")
	}
	utxo, ok := k.GetUTXO(ctx, txId)
	if !ok {
		return nil, fmt.Errorf("transaction ID is not known")
	}

	/*
		Creating a Bitcoin transaction one step at a time:
			1. Create the transaction message
			2. Get the output of the deposit transaction and convert it into the transaction input
			3. Create a new output
		See https://blog.hlongvu.com/post/t0xx5dejn3-Understanding-btcd-Part-4-Create-and-Sign-a-Bitcoin-transaction-with-btcd
	*/

	tx := wire.NewMsgTx(wire.TxVersion)

	outPoint := wire.NewOutPoint(utxo.Hash, utxo.VoutIdx)
	// The signature script will be set later and we have no witness
	txIn := wire.NewTxIn(outPoint, nil, nil)
	tx.AddTxIn(txIn)

	// msg.ValidateBasic will be called before the handler, so there is no way we have a malformed address here
	destination, _ := msg.Destination.Convert()
	addrScript, err := txscript.PayToAddrScript(destination)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "could not create pay-to-address script for destination address")
	}
	txOut := wire.NewTxOut(int64(msg.Amount), addrScript)
	tx.AddTxOut(txOut)

	k.SetRawTx(ctx, txId, tx)

	// Print out the hash that becomes the input for the threshold signing
	hash, err := txscript.CalcSignatureHash(utxo.Address.PkScript(), txscript.SigHashAll, tx, 0)
	k.Logger(ctx).Info(fmt.Sprintf("bitcoin tx to sign: %s", k.Codec().MustMarshalJSON(hash)))
	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeModule),
			sdk.NewAttribute(sdk.AttributeKeySender, msg.Sender.String()),
			sdk.NewAttribute(types.AttributeTxId, txId),
			sdk.NewAttribute(types.AttributeAmount, msg.Amount.String()),
			sdk.NewAttribute(types.AttributeDestination, msg.Destination.String()),
		),
	)

	return &sdk.Result{
		Data:   hash,
		Log:    fmt.Sprintf("successfully created withdraw transaction for Bitcoin. Hash to sign: %s", k.Codec().MustMarshalJSON(hash)),
		Events: ctx.EventManager().Events(),
	}, nil
}

func handleMsgWithdraw(ctx sdk.Context, k keeper.Keeper, rpc types.RPCClient, signer types.Signer, msg types.MsgWithdraw) (*sdk.Result, error) {
	sigScript, err := createSigScript(ctx, signer, msg.SignatureID, msg.KeyID)
	if err != nil {
		return nil, err
	}

	rawTx := k.GetRawTx(ctx, msg.TxID)
	if rawTx == nil {
		return nil, fmt.Errorf("withdraw tx for ID %s has not been prepared yet", msg.TxID)
	}

	rawTx.TxIn[0].SignatureScript = sigScript
	pkScript, err := getPkScript(ctx, k, msg.TxID)
	if err != nil {
		return nil, err
	}

	if err := validateWithdrawTx(rawTx, pkScript); err != nil {
		return nil, err
	}

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeModule),
			sdk.NewAttribute(sdk.AttributeKeySender, msg.Sender.String()),
			sdk.NewAttribute(types.AttributeTxId, msg.TxID),
			sdk.NewAttribute(types.AttributeSigId, msg.SignatureID),
		),
	)

	// This is beyond axelar's control, so we can only log the error and move on regardless
	if _, err := rpc.SendRawTransaction(rawTx, false); err != nil {
		k.Logger(ctx).Error(sdkerrors.Wrap(err, "sending transaction to Bitcoin failed").Error())
		return &sdk.Result{
			Log:    "failed to send transaction to Bitcoin (other nodes might have succeeded)",
			Events: ctx.EventManager().Events(),
		}, nil
	}

	return &sdk.Result{
		Log:    "successfully sent withdraw transaction to Bitcoin",
		Events: ctx.EventManager().Events(),
	}, nil
}

func createSigScript(ctx sdk.Context, signer types.Signer, sigId, keyId string) ([]byte, error) {
	r, s, err := signer.GetSig(ctx, sigId)
	if err != nil {
		return nil, fmt.Errorf("signature not found")
	}
	sig := btcec.Signature{
		R: r,
		S: s,
	}
	sigBytes := append(sig.Serialize(), byte(txscript.SigHashAll))

	pk, err := signer.GetKey(ctx, keyId)
	if err != nil {
		return nil, fmt.Errorf("key not found")
	}
	key := btcec.PublicKey(pk)
	keyBytes := key.SerializeUncompressed()

	sigScript, err := txscript.NewScriptBuilder().AddData(sigBytes).AddData(keyBytes).Script()
	if err != nil {
		return nil, sdkerrors.Wrap(err, "could not create bitcoin signature script")
	}
	return sigScript, nil
}

func validateWithdrawTx(withdrawTx *wire.MsgTx, pkScript []byte) error {
	flags := txscript.StandardVerifyFlags

	// execute (dry-run) the public key and signature script to validate them
	scriptEngine, err := txscript.NewEngine(pkScript, withdrawTx, 0, flags, nil, nil, withdrawTx.TxOut[0].Value)
	if err != nil {
		return sdkerrors.Wrap(err, "could not create execution engine, aborting")
	}
	if err := scriptEngine.Execute(); err != nil {
		return sdkerrors.Wrap(err, "transaction failed to execute, aborting")
	}
	return nil
}

func getPkScript(ctx sdk.Context, k keeper.Keeper, txId string) ([]byte, error) {
	utxo, ok := k.GetUTXO(ctx, txId)
	if !ok {
		return nil, fmt.Errorf("transaction ID is not known")
	}
	return utxo.Address.PkScript(), nil
}

func trackAddress(ctx sdk.Context, k keeper.Keeper, rpc types.RPCClient, address string, rescan bool) {

	// Importing an address takes a long time, therefore it cannot be done in the critical path
	go func(logger log.Logger) {
		if rescan {
			logger.Debug("Rescanning entire Bitcoin blockchain for past transactions. This will take a while.")
		}
		if err := rpc.ImportAddressRescan(address, "", rescan); err != nil {
			logger.Error(fmt.Sprintf("Could not track address %v", address))
		} else {
			logger.Debug(fmt.Sprintf("successfully tracked address %v", address))
		}
		// ctx might not be valid anymore when err is returned, so closing over logger to be safe
	}(k.Logger(ctx))

	k.SetTrackedAddress(ctx, address)
}

func addressFromKey(key ecdsa.PublicKey, chain types.Chain) (*btcutil.AddressPubKey, error) {
	btcPK := btcec.PublicKey(key)

	// For compatibility we use the uncompressed key as the basis for address generation.
	// Could be changed in the future to decrease tx size
	return btcutil.NewAddressPubKey(btcPK.SerializeUncompressed(), chain.Params())
}

func verifyTx(rpc types.RPCClient, utxo types.UTXO, expectedConfirmationHeight uint64) error {
	actualTx, err := rpc.GetRawTransactionVerbose(utxo.Hash)
	if err != nil {
		return sdkerrors.Wrap(err, "could not retrieve Bitcoin transaction")
	}

	if uint32(len(actualTx.Vout)) <= utxo.VoutIdx {
		return fmt.Errorf("vout index out of range")
	}

	vout := actualTx.Vout[utxo.VoutIdx]

	if len(vout.ScriptPubKey.Addresses) > 1 {
		return fmt.Errorf("deposit must be only spendable by a single address")
	}
	if vout.ScriptPubKey.Addresses[0] != utxo.Address.String() {
		return fmt.Errorf("expected destination address does not match actual destination address")
	}

	actualAmount, err := btcutil.NewAmount(vout.Value)
	if err != nil {
		return sdkerrors.Wrap(err, "could not parse transaction amount of the Bitcoin response")
	}
	if actualAmount != utxo.Amount {
		return fmt.Errorf("expected amount does not match actual amount")
	}

	if actualTx.Confirmations < expectedConfirmationHeight {
		return fmt.Errorf("not enough confirmations yet")
	}

	return nil
}