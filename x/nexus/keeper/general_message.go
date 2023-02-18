package keeper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/cosmos/cosmos-sdk/store/prefix"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"

	"github.com/axelarnetwork/axelar-core/utils"
	"github.com/axelarnetwork/axelar-core/utils/key"
	"github.com/axelarnetwork/axelar-core/x/nexus/exported"
	"github.com/axelarnetwork/axelar-core/x/nexus/types"
	"github.com/axelarnetwork/utils/funcs"
	"github.com/axelarnetwork/utils/slices"
)

func getMessageKey(id string) key.Key {
	return generalMessagePrefix.Append(key.FromStr(id))
}

func getSentMessageKey(destinationChain exported.ChainName, id string) key.Key {
	return sentMessagePrefix.Append(key.From(destinationChain)).Append(key.FromStr(id))
}

// GenerateMessageID generates a unique general message ID
func (k Keeper) GenerateMessageID(ctx sdk.Context) string {
	counter := utils.NewCounter[uint](messageNonceKey, k.getStore(ctx))
	nonce := counter.Incr(ctx)

	hash := sha256.Sum256(ctx.TxBytes())
	return fmt.Sprintf("%s-%d", hex.EncodeToString(hash[:]), nonce)
}

// SetNewMessage sets the given general message. If the messages is approved, adds the message ID to approved messages store
func (k Keeper) SetNewMessage(ctx sdk.Context, m exported.GeneralMessage) error {
	sourceChain, ok := k.GetChain(ctx, m.GetSourceChain())
	if !ok {
		return fmt.Errorf("source chain %s is not a registered chain", m.GetSourceChain())
	}

	validator := k.GetRouter().GetAddressValidator(sourceChain.Module)
	if err := validator(ctx, m.Sender); err != nil {
		return err
	}

	destChain, ok := k.GetChain(ctx, m.GetDestinationChain())
	if !ok {
		return fmt.Errorf("destination chain %s is not a registered chain", m.GetDestinationChain())
	}

	validator = k.GetRouter().GetAddressValidator(destChain.Module)
	if err := validator(ctx, m.Recipient); err != nil {
		return err
	}

	if m.Asset != nil {
		if err := k.validateTransferAsset(ctx, sourceChain, m.Asset.Denom); err != nil {
			return err
		}

		if err := k.validateTransferAsset(ctx, destChain, m.Asset.Denom); err != nil {
			return err
		}
	}

	if _, found := k.GetMessage(ctx, m.ID); found {
		return fmt.Errorf("general message %s already exists", m.ID)
	}

	if m.Is(exported.Sent) {
		if err := k.setSentMessageID(ctx, m); err != nil {
			return err
		}
	}

	funcs.MustNoErr(ctx.EventManager().EmitTypedEvent(&types.MessageReceived{
		ID:          m.ID,
		PayloadHash: m.PayloadHash,
		Sender:      m.Sender,
		Recipient:   m.Recipient,
	}))

	return k.setMessage(ctx, m)
}

/*
 * Below are the valid message status transitions:
 * Approved -> Sent
 * Sent -> Executed
 * Sent -> Failed
 * Failed -> Sent
 */

// SetMessageSent sets the general message as sent
func (k Keeper) SetMessageSent(ctx sdk.Context, id string) error {
	m, found := k.GetMessage(ctx, id)
	if !found {
		return fmt.Errorf("general message %s not found", id)
	}

	if !(m.Is(exported.Approved) || m.Is(exported.Failed)) {
		return fmt.Errorf("general message is not approved or failed")
	}

	m.Status = exported.Sent

	if err := k.setMessage(ctx, m); err != nil {
		return err
	}

	return k.setSentMessageID(ctx, m)
}

// SetMessageExecuted sets the general message as executed
func (k Keeper) SetMessageExecuted(ctx sdk.Context, id string) error {
	m, found := k.GetMessage(ctx, id)
	if !found {
		return fmt.Errorf("general message %s not found", id)
	}

	if !m.Is(exported.Sent) {
		return fmt.Errorf("general message is not sent or approved")
	}

	k.deleteSentMessageID(ctx, m)

	m.Status = exported.Executed

	return k.setMessage(ctx, m)
}

// SetMessageFailed sets the general message as failed
func (k Keeper) SetMessageFailed(ctx sdk.Context, id string) error {
	m, found := k.GetMessage(ctx, id)
	if !found {
		return fmt.Errorf("general message %s not found", id)
	}

	if !m.Is(exported.Sent) {
		return fmt.Errorf("general message is not sent")
	}

	k.deleteSentMessageID(ctx, m)

	m.Status = exported.Failed

	return k.setMessage(ctx, m)
}

// DeleteMessage deletes the general message with associated ID, and also deletes the message ID from the approved messages store
func (k Keeper) DeleteMessage(ctx sdk.Context, id string) {
	m, found := k.GetMessage(ctx, id)
	if found {
		k.deleteSentMessageID(ctx, m)
		k.getStore(ctx).DeleteNew(getMessageKey(id))
	}
}

// GetMessage returns the general message by ID
func (k Keeper) GetMessage(ctx sdk.Context, id string) (m exported.GeneralMessage, found bool) {
	return m, k.getStore(ctx).GetNew(getMessageKey(id), &m)
}

func (k Keeper) setMessage(ctx sdk.Context, m exported.GeneralMessage) error {
	return k.getStore(ctx).SetNewValidated(getMessageKey(m.ID), &m)
}

func (k Keeper) setSentMessageID(ctx sdk.Context, m exported.GeneralMessage) error {
	if !m.Is(exported.Sent) {
		return fmt.Errorf("general message is not sent")
	}

	k.getStore(ctx).SetRawNew(getSentMessageKey(m.GetDestinationChain(), m.ID), []byte(m.ID))
	return nil
}

func (k Keeper) deleteSentMessageID(ctx sdk.Context, m exported.GeneralMessage) {
	k.getStore(ctx).DeleteNew(getSentMessageKey(m.GetDestinationChain(), m.ID))
}

//nolint:unused // TODO: add genesis import/export
func (k Keeper) getMessages(ctx sdk.Context) (generalMessages []exported.GeneralMessage) {
	iter := k.getStore(ctx).IteratorNew(generalMessagePrefix)
	defer utils.CloseLogError(iter, k.Logger(ctx))

	for ; iter.Valid(); iter.Next() {
		var generalMessage exported.GeneralMessage
		iter.UnmarshalValue(&generalMessage)

		generalMessages = append(generalMessages, generalMessage)
	}

	return generalMessages
}

// GetSentMessages returns up to limit sent messages where chain is the destination chain
func (k Keeper) GetSentMessages(ctx sdk.Context, chain exported.ChainName, limit int64) []exported.GeneralMessage {
	ids := []string{}

	pageRequest := &query.PageRequest{
		Key:        nil,
		Offset:     0,
		Limit:      uint64(limit),
		CountTotal: false,
		Reverse:    false,
	}
	keyPrefix := append(sentMessagePrefix.Append(key.From(chain)).Bytes(), []byte(key.DefaultDelimiter)...)

	// it's unexpected to get a retrieval/iterator error from IAVL db
	funcs.Must(query.Paginate(prefix.NewStore(k.getStore(ctx).KVStore, keyPrefix), pageRequest, func(key []byte, value []byte) error {
		ids = append(ids, string(value))
		return nil
	}))

	return slices.Map(ids, func(id string) exported.GeneralMessage {
		return funcs.MustOk(k.GetMessage(ctx, id))
	})
}