package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/terastal-hq/proxylib/accountid"
	"github.com/terastal-hq/proxylib/accounts"
	"github.com/terastal-hq/proxylib/configfile"
)

// ErrUnloadableConfig means an account update was rejected because the resulting
// configuration would fail the same validation Load applies at startup.
var ErrUnloadableConfig = configfile.ErrUnloadableConfig

// Credentials represents a complete token and account identity replacement.
type Credentials struct {
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	IDToken      string `json:"idToken,omitempty"`
	AccountID    string `json:"accountId,omitempty"`
	AccountEmail string `json:"accountEmail,omitempty"`
	AccountName  string `json:"accountName,omitempty"`
	PlanType     string `json:"planType,omitempty"`
}

// AccountKey represents an API key owned by an account.
type AccountKey = accounts.Key

// Account holds the configuration, ChatGPT OAuth credentials, and API keys for one account.
type Account struct {
	ID           string       `json:"id"`
	Label        string       `json:"label,omitempty"`
	Enabled      bool         `json:"enabled"`
	RefreshToken string       `json:"refreshToken,omitempty"`
	AccessToken  string       `json:"accessToken,omitempty"`
	IDToken      string       `json:"idToken,omitempty"`
	AccountID    string       `json:"accountId,omitempty"`
	AccountEmail string       `json:"accountEmail,omitempty"`
	AccountName  string       `json:"accountName,omitempty"`
	PlanType     string       `json:"planType,omitempty"`
	Keys         []AccountKey `json:"keys,omitempty"`
}

func (acc Account) GetID() string           { return acc.ID }
func (acc Account) GetUpstreamID() string   { return acc.AccountID }
func (acc Account) GetKeys() []accounts.Key { return acc.Keys }
func (acc Account) GetLabel() string        { return acc.Label }
func (acc Account) GetEmail() string        { return acc.AccountEmail }
func (acc Account) IsEnabled() bool         { return acc.Enabled }
func (acc *Account) SetLabel(l string)      { acc.Label = l }
func (acc *Account) SetEnabled(e bool)      { acc.Enabled = e }
func (acc *Account) AddKey(k accounts.Key)  { acc.Keys = append(acc.Keys, k) }
func (acc *Account) RemoveKey(keyID string) bool {
	filtered := make([]accounts.Key, 0, len(acc.Keys))
	found := false
	for _, k := range acc.Keys {
		if k.ID == keyID {
			found = true
			continue
		}
		filtered = append(filtered, k)
	}
	if found {
		acc.Keys = filtered
	}
	return found
}

// ApplyCredentials updates an account's token and ChatGPT account identity fields.
func (acc *Account) ApplyCredentials(creds Credentials) {
	acc.AccessToken = creds.AccessToken
	acc.RefreshToken = creds.RefreshToken
	acc.IDToken = creds.IDToken
	acc.AccountID = creds.AccountID
	acc.AccountEmail = creds.AccountEmail
	acc.AccountName = creds.AccountName
	if creds.PlanType != "" {
		acc.PlanType = creds.PlanType
	}
}

// HasCredentials returns true if the account has an access or refresh token.
func (acc Account) HasCredentials() bool {
	return acc.AccessToken != "" || acc.RefreshToken != ""
}

// ValidateAccountID checks whether id satisfies the account slug requirements:
// 1-32 characters, lowercase alphanumeric and hyphens, starting and ending with alphanumeric.
func ValidateAccountID(id string) error {
	return accountid.Validate(id)
}

// DeriveAccountID generates a unique, valid account slug from an email address.
func DeriveAccountID(email string, existingIDs []string) string {
	return accountid.Derive(email, existingIDs)
}

// ValidateAccounts validates a collection of accounts for uniqueness and key integrity.
func ValidateAccounts(accts []Account) error {
	return accounts.Validate(accts, "ChatGPT")
}

// ReadDiskAccounts loads accounts directly from the configuration file at path.
func ReadDiskAccounts(path string) ([]Account, error) {
	var disk Config
	_, err := readConfigFile(path, &disk)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return disk.Accounts, nil
}

var legacyCredentialKeys = []string{
	"accessToken", "refreshToken", "idToken", "accountId",
	"accountEmail", "accountName", "apiKey", "planType",
}

// UpdateAccounts reads the configuration at path, applies update to the accounts,
// validates the result, and durably persists it while stripping legacy flat credential fields.
func UpdateAccounts(path string, update func(accounts *[]Account) error) error {
	var disk Config
	data, err := readConfigFile(path, &disk)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	accounts := disk.Accounts
	if len(accounts) == 0 && disk.hasLegacyCredentials() {
		accounts = disk.migratedLegacyAccounts()
	}

	if err := update(&accounts); err != nil {
		return err
	}

	if err := ValidateAccounts(accounts); err != nil {
		return fmt.Errorf("validate accounts: %w", err)
	}

	return configfile.PatchWithData(path, data, func(fields map[string]json.RawMessage) error {
		accountsJSON, err := json.MarshalIndent(accounts, "  ", "  ")
		if err != nil {
			return fmt.Errorf("encode accounts: %w", err)
		}
		fields["accounts"] = accountsJSON
		configfile.StripKeys(fields, legacyCredentialKeys...)
		return nil
	}, func(candidate []byte) error {
		return validateAsLoaded(candidate, filepath.Dir(path))
	})
}

// UpdateAdminKey updates the adminKeyHash in the configuration file at path.
func UpdateAdminKey(path string, hash string) error {
	var disk Config
	data, err := readConfigFile(path, &disk)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return configfile.PatchWithData(path, data, func(fields map[string]json.RawMessage) error {
		hashJSON, err := json.Marshal(hash)
		if err != nil {
			return fmt.Errorf("encode admin key hash: %w", err)
		}
		fields["adminKeyHash"] = hashJSON
		return nil
	}, nil)
}

func validateAsLoaded(data []byte, dir string) error {
	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return err
	}
	cfg.normalize()
	return cfg.Validate()
}
