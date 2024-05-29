package yandex

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/lockbox/v1"
	"golang.org/x/crypto/scrypt"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	yandexLockboxSecretVersionHashedDefaultTimeout = 1 * time.Minute
	maxSafeEntries                                 = 10
)

func resourceYandexLockboxSecretVersionHashed() *schema.Resource {
	return &schema.Resource{
		ReadContext:   resourceYandexLockboxSecretVersionHashedRead,
		CreateContext: resourceYandexLockboxSecretVersionHashedCreate,
		DeleteContext: resourceYandexLockboxSecretVersionHashedDelete,
		// UpdateContext: nil, // updates are not supported, all fields have ForceNew: true

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(yandexLockboxSecretVersionHashedDefaultTimeout),
			Read:   schema.DefaultTimeout(yandexLockboxSecretVersionHashedDefaultTimeout),
			Delete: schema.DefaultTimeout(yandexLockboxSecretVersionHashedDefaultTimeout),
		},

		SchemaVersion: 1,

		Schema: addSafeEntries(maxSafeEntries, map[string]*schema.Schema{
			"secret_id": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringLenBetween(0, 50),
			},

			"description": {
				Type:         schema.TypeString,
				Optional:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringLenBetween(0, 1024),
			},
		}),
	}
}

func resourceYandexLockboxSecretVersionHashedCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	config := meta.(*Config)

	versionPayloadEntries, err := expandLockboxSecretVersionSafeEntries(d)
	if err != nil {
		return diag.FromErr(err)
	}

	getPayloadReq := &lockbox.GetPayloadRequest{
		SecretId: d.Get("secret_id").(string),
		// It's not relevant what version to use as base, since addEntryChangesForRemovedKeys will just leave the versionPayloadEntries.
		// The current entries will be ignored. The behaviour is like PayloadChangeKind.FULL in ycp provider.
	}

	log.Printf("[INFO] getting Lockbox payload (to compare entries): %s", protojson.Format(getPayloadReq))

	payload, err := config.sdk.LockboxPayload().Payload().Get(ctx, getPayloadReq)
	if err != nil {
		return diag.Errorf("could not get payload from secret %v and version %v: %s", getPayloadReq.SecretId, getPayloadReq.VersionId, err)
	}

	log.Printf("[INFO] read Lockbox payload (to compare entries) with VersionID: %s", payload.GetVersionId())

	req := &lockbox.AddVersionRequest{
		SecretId: d.Get("secret_id").(string),
		// Make sure we're taking this version as reference, since addEntryChangesForRemovedKeys will
		// remove the entries in payload.Entries that versionPayloadEntries doesn't contain anymore.
		BaseVersionId:  payload.VersionId,
		Description:    d.Get("description").(string),
		PayloadEntries: addEntryChangesForRemovedKeys(payload.Entries, versionPayloadEntries),
	}

	log.Printf("[INFO] adding Lockbox version for secret with ID: %s, base version ID: %s", req.SecretId, req.BaseVersionId)

	op, err := config.sdk.WrapOperation(config.sdk.LockboxSecret().Secret().AddVersion(ctx, req))
	if err != nil {
		return diag.Errorf("error while requesting API to add version: %s", err)
	}

	protoMetadata, err := op.Metadata()
	if err != nil {
		return diag.Errorf("error while getting operation metadata of add secret version: %s", err)
	}

	md, ok := protoMetadata.(*lockbox.AddVersionMetadata)
	if !ok {
		return diag.Errorf("could not get Secret ID from create operation metadata")
	}

	d.SetId(md.VersionId)

	err = op.Wait(ctx)
	if err != nil {
		return diag.Errorf("error while waiting operation to add secret version: %s", err)
	}

	if _, err := op.Response(); err != nil {
		return diag.Errorf("add secret version failed: %s", err)
	}

	log.Printf("[INFO] added Lockbox version with ID: %s", d.Id())

	return resourceYandexLockboxSecretVersionHashedRead(ctx, d, meta)
}

func resourceYandexLockboxSecretVersionHashedRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	config := meta.(*Config)

	id := d.Id()
	req := &lockbox.GetPayloadRequest{
		SecretId:  d.Get("secret_id").(string),
		VersionId: id,
	}

	log.Printf("[INFO] reading Lockbox version: %s", protojson.Format(req))

	_, err := config.sdk.LockboxPayload().Payload().Get(ctx, req)
	if err != nil {
		return diag.FromErr(handleNotFoundError(err, d, fmt.Sprintf("secret version payload %q", id)))
	}

	log.Printf("[INFO] read Lockbox version with ID: %s", id)

	return nil
}

func resourceYandexLockboxSecretVersionHashedDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	// TODO - we could call ScheduleVersionDestruction
	log.Printf("[INFO] Versions are only deleted when secret is deleted, version ID: %s", d.Id())
	return nil
}

// Instead of `entries`, we add key_X/text_value_X; text_value(s) will be hashed in state.
func addSafeEntries(n int, schemaMap map[string]*schema.Schema) map[string]*schema.Schema {
	for i := 1; i <= n; i++ {
		// schema properties were taken from "entries" in the original lockbox_secret_version
		schemaMap[keyName(i)] = &schema.Schema{
			Type:         schema.TypeString,
			Optional:     true, // here key must be optional, since only some keys will be used
			ForceNew:     true,
			ValidateFunc: validation.All(validation.StringMatch(regexp.MustCompile(`^([-_./\\@0-9a-zA-Z]+)$`), ""), validation.StringLenBetween(0, 256)),
		}
		schemaMap[textValueName(i)] = &schema.Schema{
			Type:         schema.TypeString,
			Optional:     true,
			ForceNew:     true,
			Sensitive:    true,
			ValidateFunc: validation.StringLenBetween(0, 65536),
			StateFunc:    hashPayloadTextValue, // hide this sensitive value
			RequiredWith: []string{keyName(i)},
		}
	}

	return schemaMap
}

func keyName(i int) string {
	return fmt.Sprintf("key_%d", i)
}

func textValueName(i int) string {
	return fmt.Sprintf("text_value_%d", i)
}

// We use Scrypt because it's a hash algorithm that lets you configure an arbitrary difficulty,
// and the result is deterministic (Terraform requires that values don't change between runs).
// Other options that don't have these features:
// - SHA-256: it's deterministic, but has a fixed difficulty.
// - Bcrypt: you can parametrize difficulty, but the result is not deterministic.
func hashPayloadTextValue(i interface{}) string {
	textValue := i.(string)
	if textValue == "" {
		return ""
	}
	keyLength := 128                                 // select reasonable length
	salt := []byte("|82&pvyYC[el3Z([,En#1:£!VJ2fKz") // this salt is public, but I guess it's better than nothing
	// scrypt.Key recommends N=32768, r=8 and p=1 (in my Macbook 2*32768 exceeds 100ms)
	hash, err := scrypt.Key([]byte(textValue), salt, 32768, 8, 1, keyLength)
	if err != nil {
		log.Printf("[ERROR] could not hash value: %v", err)
		return ""
	}
	hashBase64 := base64.StdEncoding.EncodeToString(hash)
	return hashBase64
}

func expandLockboxSecretVersionSafeEntries(d *schema.ResourceData) ([]*lockbox.PayloadEntryChange, error) {
	result := make([]*lockbox.PayloadEntryChange, 0)

	firstKeyNotFound := ""
	for i := 1; i <= maxSafeEntries; i++ {
		entry, err := getVersionPayloadEntry(d, i)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			if firstKeyNotFound != "" {
				return nil, fmt.Errorf("found %s, but previous key %s doesn't exist", keyName(i), firstKeyNotFound)
			}
			result = append(result, entry)
		} else {
			firstKeyNotFound = keyName(i)
		}
	}

	return result, nil
}

// returns the entry i (e.g. for i=1 it's key_1, text_value_1), or nil if not found
func getVersionPayloadEntry(d *schema.ResourceData, i int) (*lockbox.PayloadEntryChange, error) {
	entryKeyName := keyName(i)
	entryKey, exists := d.GetOk(entryKeyName)
	if !exists {
		return nil, nil // it's not an error, just that the key was not found
	}
	entry := new(lockbox.PayloadEntryChange)
	entry.SetKey(entryKey.(string))
	text, exists := d.GetOk(textValueName(i))
	if !exists {
		return nil, fmt.Errorf("%s exists but there is no corresponding %s", entryKeyName, textValueName(i))
	}
	entry.SetTextValue(text.(string))
	return entry, nil
}
