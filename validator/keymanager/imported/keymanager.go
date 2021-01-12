package imported

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	validatorpb "github.com/prysmaticlabs/prysm/proto/validator/accounts/v2"
	"github.com/prysmaticlabs/prysm/shared/bls"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/event"
	"github.com/prysmaticlabs/prysm/shared/interop"
	"github.com/prysmaticlabs/prysm/shared/petnames"
	"github.com/prysmaticlabs/prysm/validator/accounts/iface"
	"github.com/sirupsen/logrus"
	keystorev4 "github.com/wealdtech/go-eth2-wallet-encryptor-keystorev4"
	"go.opencensus.io/trace"
)

var (
	lock              sync.RWMutex
	orderedPublicKeys = make([][48]byte, 0)
	secretKeysCache   = make(map[[48]byte]bls.SecretKey)
)

const (
	// KeystoreFileNameFormat exposes the filename the keystore should be formatted in.
	KeystoreFileNameFormat = "keystore-%d.json"
	// AccountsPath where all imported keymanager keystores are kept.
	AccountsPath = "accounts"
	// AccountsKeystoreFileName exposes the name of the keystore file.
	AccountsKeystoreFileName = "all-accounts.keystore.json"
)

// Keymanager implementation for imported keystores utilizing EIP-2335.
type Keymanager struct {
	wallet              iface.Wallet
	accountsStore       *accountStore
	disabledPublicKeys  map[[48]byte]bool
	accountsChangedFeed *event.Feed
}

// SetupConfig includes configuration values for initializing
// a keymanager, such as passwords, the wallet, and more.
type SetupConfig struct {
	Wallet iface.Wallet
}

// Defines a struct containing 1-to-1 corresponding
// private keys and public keys for eth2 validators.
type accountStore struct {
	PrivateKeys [][]byte `json:"private_keys"`
	PublicKeys  [][]byte `json:"public_keys"`
}

// AccountsKeystoreRepresentation defines an internal Prysm representation
// of validator accounts, encrypted according to the EIP-2334 standard
// but containing extra fields such as markers for disabled public keys.
type AccountsKeystoreRepresentation struct {
	Crypto             map[string]interface{} `json:"crypto"`
	ID                 string                 `json:"uuid"`
	Version            uint                   `json:"version"`
	Name               string                 `json:"name"`
	DisabledPublicKeys []string               `json:"disabled_public_keys"`
}

// ResetCaches for the keymanager.
func ResetCaches() {
	lock.Lock()
	orderedPublicKeys = make([][48]byte, 0)
	secretKeysCache = make(map[[48]byte]bls.SecretKey)
	lock.Unlock()
}

// NewKeymanager instantiates a new imported keymanager from configuration options.
func NewKeymanager(ctx context.Context, cfg *SetupConfig) (*Keymanager, error) {
	k := &Keymanager{
		wallet:              cfg.Wallet,
		accountsStore:       &accountStore{},
		accountsChangedFeed: new(event.Feed),
		disabledPublicKeys:  make(map[[48]byte]bool),
	}

	if err := k.initializeAccountKeystore(ctx); err != nil {
		return nil, errors.Wrap(err, "failed to initialize account store")
	}

	// We begin a goroutine to listen for file changes to our
	// all-accounts.keystore.json file in the wallet directory.
	go k.listenForAccountChanges(ctx)
	return k, nil
}

// NewInteropKeymanager instantiates a new imported keymanager with the deterministically generated interop keys.
func NewInteropKeymanager(_ context.Context, offset, numValidatorKeys uint64) (*Keymanager, error) {
	k := &Keymanager{
		accountsChangedFeed: new(event.Feed),
	}
	if numValidatorKeys == 0 {
		return k, nil
	}
	secretKeys, publicKeys, err := interop.DeterministicallyGenerateKeys(offset, numValidatorKeys)
	if err != nil {
		return nil, errors.Wrap(err, "could not generate interop keys")
	}
	lock.Lock()
	pubKeys := make([][48]byte, numValidatorKeys)
	for i := uint64(0); i < numValidatorKeys; i++ {
		publicKey := bytesutil.ToBytes48(publicKeys[i].Marshal())
		pubKeys[i] = publicKey
		secretKeysCache[publicKey] = secretKeys[i]
	}
	orderedPublicKeys = pubKeys
	lock.Unlock()
	return k, nil
}

// SubscribeAccountChanges creates an event subscription for a channel
// to listen for public key changes at runtime, such as when new validator accounts
// are imported into the keymanager while the validator process is running.
func (dr *Keymanager) SubscribeAccountChanges(pubKeysChan chan [][48]byte) event.Subscription {
	return dr.accountsChangedFeed.Subscribe(pubKeysChan)
}

// ValidatingAccountNames for a imported keymanager.
func (dr *Keymanager) ValidatingAccountNames() ([]string, error) {
	lock.RLock()
	names := make([]string, len(orderedPublicKeys))
	for i, pubKey := range orderedPublicKeys {
		names[i] = petnames.DeterministicName(bytesutil.FromBytes48(pubKey), "-")
	}
	lock.RUnlock()
	return names, nil
}

// DisabledPublicKeys returns the currently disabled public keys in the keymanager.
func (dr *Keymanager) DisabledPublicKeys() [][]byte {
	disabledPubKeys := make([][]byte, 0)
	for pubKey := range dr.disabledPublicKeys {
		pubKeyBytes := make([]byte, 48)
		copy(pubKeyBytes, pubKey[:])
		disabledPubKeys = append(disabledPubKeys, pubKeyBytes)
	}
	return disabledPubKeys
}

// Initialize public and secret key caches that are used to speed up the functions
// FetchValidatingPublicKeys and Sign
func (dr *Keymanager) initializeKeysCachesFromKeystore() error {
	lock.Lock()
	defer lock.Unlock()
	count := len(dr.accountsStore.PrivateKeys)
	orderedPublicKeys = make([][48]byte, count)
	secretKeysCache = make(map[[48]byte]bls.SecretKey, count)
	for i, publicKey := range dr.accountsStore.PublicKeys {
		publicKey48 := bytesutil.ToBytes48(publicKey)
		orderedPublicKeys[i] = publicKey48
		secretKey, err := bls.SecretKeyFromBytes(dr.accountsStore.PrivateKeys[i])
		if err != nil {
			return errors.Wrap(err, "failed to initialize keys caches from account keystore")
		}
		secretKeysCache[publicKey48] = secretKey
	}
	return nil
}

// DeleteAccounts takes in public keys and removes the accounts entirely. This includes their disk keystore and cached keystore.
func (dr *Keymanager) DeleteAccounts(ctx context.Context, publicKeys [][]byte) error {
	for _, publicKey := range publicKeys {
		var index int
		var found bool
		for i, pubKey := range dr.accountsStore.PublicKeys {
			if bytes.Equal(pubKey, publicKey) {
				index = i
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("could not find public key %#x", publicKey)
		}
		deletedPublicKey := dr.accountsStore.PublicKeys[index]
		accountName := petnames.DeterministicName(deletedPublicKey, "-")
		dr.accountsStore.PrivateKeys = append(dr.accountsStore.PrivateKeys[:index], dr.accountsStore.PrivateKeys[index+1:]...)
		dr.accountsStore.PublicKeys = append(dr.accountsStore.PublicKeys[:index], dr.accountsStore.PublicKeys[index+1:]...)

		newStore, err := dr.CreateAccountsKeystore(ctx, dr.accountsStore.PrivateKeys, dr.accountsStore.PublicKeys)
		if err != nil {
			return errors.Wrap(err, "could not rewrite accounts keystore")
		}

		// Write the encoded keystore.
		encoded, err := json.MarshalIndent(newStore, "", "\t")
		if err != nil {
			return err
		}
		if err := dr.wallet.WriteFileAtPath(ctx, AccountsPath, AccountsKeystoreFileName, encoded); err != nil {
			return errors.Wrap(err, "could not write keystore file for accounts")
		}

		log.WithFields(logrus.Fields{
			"name":      accountName,
			"publicKey": fmt.Sprintf("%#x", bytesutil.Trunc(deletedPublicKey)),
		}).Info("Successfully deleted validator account")
		err = dr.initializeKeysCachesFromKeystore()
		if err != nil {
			return errors.Wrap(err, "failed to initialize keys caches")
		}
	}
	return nil
}

// FetchValidatingPublicKeys fetches the list of active public keys from the imported account keystores.
func (dr *Keymanager) FetchValidatingPublicKeys(ctx context.Context) ([][48]byte, error) {
	ctx, span := trace.StartSpan(ctx, "keymanager.FetchValidatingPublicKeys")
	defer span.End()

	lock.RLock()
	keys := orderedPublicKeys
	result := make([][48]byte, 0)
	for _, pk := range keys {
		if _, ok := dr.disabledPublicKeys[pk]; !ok {
			result = append(result, pk)
		}
	}
	lock.RUnlock()
	return result, nil
}

// FetchAllValidatingPublicKeys fetches the list of all public keys (including disabled ones) from the imported account keystores.
func (dr *Keymanager) FetchAllValidatingPublicKeys(ctx context.Context) ([][48]byte, error) {
	ctx, span := trace.StartSpan(ctx, "keymanager.FetchValidatingPublicKeys")
	defer span.End()

	lock.RLock()
	keys := orderedPublicKeys
	result := make([][48]byte, len(keys))
	copy(result, keys)
	lock.RUnlock()
	return result, nil
}

// FetchValidatingPrivateKeys fetches the list of private keys from the secret keys cache
func (dr *Keymanager) FetchValidatingPrivateKeys(ctx context.Context) ([][32]byte, error) {
	lock.RLock()
	defer lock.RUnlock()
	privKeys := make([][32]byte, len(secretKeysCache))
	pubKeys, err := dr.FetchValidatingPublicKeys(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "could not retrieve public keys")
	}
	for i, pk := range pubKeys {
		if _, ok := dr.disabledPublicKeys[pk]; !ok {
			seckey, ok := secretKeysCache[pk]
			if !ok {
				return nil, errors.New("Could not fetch private key")
			}
			privKeys[i] = bytesutil.ToBytes32(seckey.Marshal())
		}
	}
	return privKeys, nil
}

// Sign signs a message using a validator key.
func (dr *Keymanager) Sign(ctx context.Context, req *validatorpb.SignRequest) (bls.Signature, error) {
	ctx, span := trace.StartSpan(ctx, "keymanager.Sign")
	defer span.End()

	publicKey := req.PublicKey
	if publicKey == nil {
		return nil, errors.New("nil public key in request")
	}
	lock.RLock()
	secretKey, ok := secretKeysCache[bytesutil.ToBytes48(publicKey)]
	lock.RUnlock()
	if !ok {
		return nil, errors.New("no signing key found in keys cache")
	}
	return secretKey.Sign(req.SigningRoot), nil
}

func (dr *Keymanager) initializeAccountKeystore(ctx context.Context) error {
	encoded, err := dr.wallet.ReadFileAtPath(ctx, AccountsPath, AccountsKeystoreFileName)
	if err != nil && strings.Contains(err.Error(), "no files found") {
		// If there are no keys to initialize at all, just exit.
		return nil
	} else if err != nil {
		return errors.Wrapf(err, "could not read keystore file for accounts %s", AccountsKeystoreFileName)
	}
	keystoreFile := &AccountsKeystoreRepresentation{}
	if err := json.Unmarshal(encoded, keystoreFile); err != nil {
		return errors.Wrapf(err, "could not decode keystore file for accounts %s", AccountsKeystoreFileName)
	}
	// We extract the validator signing private key from the keystore
	// by utilizing the password and initialize a new BLS secret key from
	// its raw bytes.
	password := dr.wallet.Password()
	decryptor := keystorev4.New()
	enc, err := decryptor.Decrypt(keystoreFile.Crypto, password)
	if err != nil && strings.Contains(err.Error(), "invalid checksum") {
		return errors.Wrap(err, "wrong password for wallet entered")
	} else if err != nil {
		return errors.Wrap(err, "could not decrypt keystore")
	}

	store := &accountStore{}
	if err := json.Unmarshal(enc, store); err != nil {
		return err
	}
	if len(store.PublicKeys) != len(store.PrivateKeys) {
		return errors.New("unequal number of public keys and private keys")
	}
	if len(store.PublicKeys) == 0 {
		return nil
	}
	dr.accountsStore = store

	lock.Lock()
	for _, pubKey := range keystoreFile.DisabledPublicKeys {
		pubKeyBytes, err := hex.DecodeString(pubKey)
		if err != nil {
			lock.Unlock()
			return err
		}
		dr.disabledPublicKeys[bytesutil.ToBytes48(pubKeyBytes)] = true
	}
	lock.Unlock()
	err = dr.initializeKeysCachesFromKeystore()
	if err != nil {
		return errors.Wrap(err, "failed to initialize keys caches")
	}
	return err
}

// CreateAccountsKeystore creates a new keystore holding the provided keys.
func (dr *Keymanager) CreateAccountsKeystore(
	_ context.Context,
	privateKeys, publicKeys [][]byte,
) (*AccountsKeystoreRepresentation, error) {
	encryptor := keystorev4.New()
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	if len(privateKeys) != len(publicKeys) {
		return nil, fmt.Errorf(
			"number of private keys and public keys is not equal: %d != %d", len(privateKeys), len(publicKeys),
		)
	}
	if dr.accountsStore == nil {
		dr.accountsStore = &accountStore{
			PrivateKeys: privateKeys,
			PublicKeys:  publicKeys,
		}
	} else {
		existingPubKeys := make(map[string]bool)
		existingPrivKeys := make(map[string]bool)
		for i := 0; i < len(dr.accountsStore.PrivateKeys); i++ {
			existingPrivKeys[string(dr.accountsStore.PrivateKeys[i])] = true
			existingPubKeys[string(dr.accountsStore.PublicKeys[i])] = true
		}
		// We append to the accounts store keys only
		// if the private/secret key do not already exist, to prevent duplicates.
		for i := 0; i < len(privateKeys); i++ {
			sk := privateKeys[i]
			pk := publicKeys[i]
			_, privKeyExists := existingPrivKeys[string(sk)]
			_, pubKeyExists := existingPubKeys[string(pk)]
			if privKeyExists || pubKeyExists {
				continue
			}
			dr.accountsStore.PublicKeys = append(dr.accountsStore.PublicKeys, pk)
			dr.accountsStore.PrivateKeys = append(dr.accountsStore.PrivateKeys, sk)
		}
	}
	err = dr.initializeKeysCachesFromKeystore()
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize keys caches")
	}
	encodedStore, err := json.MarshalIndent(dr.accountsStore, "", "\t")
	if err != nil {
		return nil, err
	}
	cryptoFields, err := encryptor.Encrypt(encodedStore, dr.wallet.Password())
	if err != nil {
		return nil, errors.Wrap(err, "could not encrypt accounts")
	}
	disabledPubKeys := make([]string, 0)
	lock.Lock()
	defer lock.Unlock()
	for pubKey := range dr.disabledPublicKeys {
		disabledPubKeys = append(disabledPubKeys, fmt.Sprintf("%x", pubKey))
	}
	return &AccountsKeystoreRepresentation{
		Crypto:             cryptoFields,
		ID:                 id.String(),
		Version:            encryptor.Version(),
		Name:               encryptor.Name(),
		DisabledPublicKeys: disabledPubKeys,
	}, nil
}
