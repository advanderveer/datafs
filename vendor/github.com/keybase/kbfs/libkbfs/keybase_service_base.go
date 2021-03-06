// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"sync"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/logger"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/kbfs/kbfscrypto"
	"golang.org/x/net/context"
)

// KeybaseServiceBase implements most of KeybaseService from protocol
// defined clients.
type KeybaseServiceBase struct {
	context         Context
	identifyClient  keybase1.IdentifyInterface
	userClient      keybase1.UserInterface
	sessionClient   keybase1.SessionInterface
	favoriteClient  keybase1.FavoriteInterface
	kbfsClient      keybase1.KbfsInterface
	kbfsMountClient keybase1.KbfsMountInterface
	log             logger.Logger

	config Config

	sessionCacheLock sync.RWMutex
	// Set to the zero value when invalidated.
	cachedCurrentSession SessionInfo

	userCacheLock sync.RWMutex
	// Map entries are removed when invalidated.
	userCache               map[keybase1.UID]UserInfo
	userCacheUnverifiedKeys map[keybase1.UID][]keybase1.PublicKey

	lastNotificationFilenameLock sync.Mutex
	lastNotificationFilename     string
	lastSyncNotificationPath     string
}

// NewKeybaseServiceBase makes a new KeybaseService.
func NewKeybaseServiceBase(config Config, kbCtx Context, log logger.Logger) *KeybaseServiceBase {
	k := KeybaseServiceBase{
		config:                  config,
		context:                 kbCtx,
		log:                     log,
		userCache:               make(map[keybase1.UID]UserInfo),
		userCacheUnverifiedKeys: make(map[keybase1.UID][]keybase1.PublicKey),
	}
	return &k
}

// FillClients sets the client protocol implementations needed for a KeybaseService.
func (k *KeybaseServiceBase) FillClients(identifyClient keybase1.IdentifyInterface,
	userClient keybase1.UserInterface, sessionClient keybase1.SessionInterface,
	favoriteClient keybase1.FavoriteInterface, kbfsClient keybase1.KbfsInterface,
	kbfsMountClient keybase1.KbfsMountInterface) {
	k.identifyClient = identifyClient
	k.userClient = userClient
	k.sessionClient = sessionClient
	k.favoriteClient = favoriteClient
	k.kbfsClient = kbfsClient
	k.kbfsMountClient = kbfsMountClient
}

type addVerifyingKeyFunc func(kbfscrypto.VerifyingKey)
type addCryptPublicKeyFunc func(kbfscrypto.CryptPublicKey)

// processKey adds the given public key to the appropriate verifying
// or crypt list (as return values), and also updates the given name
// map and parent map in place.
func processKey(publicKey keybase1.PublicKey,
	addVerifyingKey addVerifyingKeyFunc,
	addCryptPublicKey addCryptPublicKeyFunc,
	kidNames map[keybase1.KID]string,
	parents map[keybase1.KID]keybase1.KID) error {
	if len(publicKey.PGPFingerprint) > 0 {
		return nil
	}
	// Import the KID to validate it.
	key, err := libkb.ImportKeypairFromKID(publicKey.KID)
	if err != nil {
		return err
	}
	if publicKey.IsSibkey {
		addVerifyingKey(kbfscrypto.MakeVerifyingKey(key.GetKID()))
	} else {
		addCryptPublicKey(kbfscrypto.MakeCryptPublicKey(key.GetKID()))
	}
	if publicKey.DeviceDescription != "" {
		kidNames[publicKey.KID] = publicKey.DeviceDescription
	}

	if publicKey.ParentID != "" {
		parentKID, err := keybase1.KIDFromStringChecked(
			publicKey.ParentID)
		if err != nil {
			return err
		}
		parents[publicKey.KID] = parentKID
	}
	return nil
}

// updateKIDNamesFromParents sets the name of each KID without a name
// that has a a parent with a name, to that parent's name.
func updateKIDNamesFromParents(kidNames map[keybase1.KID]string,
	parents map[keybase1.KID]keybase1.KID) {
	for kid, parent := range parents {
		if _, ok := kidNames[kid]; ok {
			continue
		}
		if parentName, ok := kidNames[parent]; ok {
			kidNames[kid] = parentName
		}
	}
}

func filterKeys(keys []keybase1.PublicKey) (
	[]kbfscrypto.VerifyingKey, []kbfscrypto.CryptPublicKey,
	map[keybase1.KID]string, error) {
	var verifyingKeys []kbfscrypto.VerifyingKey
	var cryptPublicKeys []kbfscrypto.CryptPublicKey
	var kidNames = map[keybase1.KID]string{}
	var parents = map[keybase1.KID]keybase1.KID{}

	addVerifyingKey := func(key kbfscrypto.VerifyingKey) {
		verifyingKeys = append(verifyingKeys, key)
	}
	addCryptPublicKey := func(key kbfscrypto.CryptPublicKey) {
		cryptPublicKeys = append(cryptPublicKeys, key)
	}

	for _, publicKey := range keys {
		err := processKey(publicKey, addVerifyingKey, addCryptPublicKey,
			kidNames, parents)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	updateKIDNamesFromParents(kidNames, parents)
	return verifyingKeys, cryptPublicKeys, kidNames, nil
}

func filterRevokedKeys(keys []keybase1.RevokedKey) (
	map[kbfscrypto.VerifyingKey]keybase1.KeybaseTime,
	map[kbfscrypto.CryptPublicKey]keybase1.KeybaseTime,
	map[keybase1.KID]string, error) {
	verifyingKeys := make(map[kbfscrypto.VerifyingKey]keybase1.KeybaseTime)
	cryptPublicKeys := make(map[kbfscrypto.CryptPublicKey]keybase1.KeybaseTime)
	var kidNames = map[keybase1.KID]string{}
	var parents = map[keybase1.KID]keybase1.KID{}

	for _, revokedKey := range keys {
		addVerifyingKey := func(key kbfscrypto.VerifyingKey) {
			verifyingKeys[key] = revokedKey.Time
		}
		addCryptPublicKey := func(key kbfscrypto.CryptPublicKey) {
			cryptPublicKeys[key] = revokedKey.Time
		}
		err := processKey(revokedKey.Key, addVerifyingKey, addCryptPublicKey,
			kidNames, parents)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	updateKIDNamesFromParents(kidNames, parents)
	return verifyingKeys, cryptPublicKeys, kidNames, nil

}

func (k *KeybaseServiceBase) getCachedCurrentSession() SessionInfo {
	k.sessionCacheLock.RLock()
	defer k.sessionCacheLock.RUnlock()
	return k.cachedCurrentSession
}

func (k *KeybaseServiceBase) setCachedCurrentSession(s SessionInfo) {
	k.sessionCacheLock.Lock()
	defer k.sessionCacheLock.Unlock()
	k.cachedCurrentSession = s
}

func (k *KeybaseServiceBase) getCachedUserInfo(uid keybase1.UID) UserInfo {
	k.userCacheLock.RLock()
	defer k.userCacheLock.RUnlock()
	return k.userCache[uid]
}

func (k *KeybaseServiceBase) setCachedUserInfo(uid keybase1.UID, info UserInfo) {
	k.userCacheLock.Lock()
	defer k.userCacheLock.Unlock()
	if info.Name == libkb.NormalizedUsername("") {
		delete(k.userCache, uid)
	} else {
		k.userCache[uid] = info
	}
}

func (k *KeybaseServiceBase) getCachedUnverifiedKeys(uid keybase1.UID) (
	[]keybase1.PublicKey, bool) {
	k.userCacheLock.RLock()
	defer k.userCacheLock.RUnlock()
	if unverifiedKeys, ok := k.userCacheUnverifiedKeys[uid]; ok {
		return unverifiedKeys, true
	}
	return nil, false
}

func (k *KeybaseServiceBase) setCachedUnverifiedKeys(uid keybase1.UID, pk []keybase1.PublicKey) {
	k.userCacheLock.Lock()
	defer k.userCacheLock.Unlock()
	k.userCacheUnverifiedKeys[uid] = pk
}

func (k *KeybaseServiceBase) clearCachedUnverifiedKeys(uid keybase1.UID) {
	k.userCacheLock.Lock()
	defer k.userCacheLock.Unlock()
	delete(k.userCacheUnverifiedKeys, uid)
}

func (k *KeybaseServiceBase) clearCaches() {
	k.setCachedCurrentSession(SessionInfo{})
	k.userCacheLock.Lock()
	defer k.userCacheLock.Unlock()
	k.userCache = make(map[keybase1.UID]UserInfo)
	k.userCacheUnverifiedKeys = make(map[keybase1.UID][]keybase1.PublicKey)
}

// LoggedIn implements keybase1.NotifySessionInterface.
func (k *KeybaseServiceBase) LoggedIn(ctx context.Context, name string) error {
	k.log.CDebugf(ctx, "Current session logged in: %s", name)
	// Since we don't have the whole session, just clear the cache.
	k.setCachedCurrentSession(SessionInfo{})
	if k.config != nil {
		serviceLoggedIn(
			ctx, k.config, name, TLFJournalBackgroundWorkEnabled)
	}
	return nil
}

// LoggedOut implements keybase1.NotifySessionInterface.
func (k *KeybaseServiceBase) LoggedOut(ctx context.Context) error {
	k.log.CDebugf(ctx, "Current session logged out")
	k.setCachedCurrentSession(SessionInfo{})
	if k.config != nil {
		serviceLoggedOut(ctx, k.config)
	}
	return nil
}

// KeyfamilyChanged implements keybase1.NotifyKeyfamilyInterface.
func (k *KeybaseServiceBase) KeyfamilyChanged(ctx context.Context,
	uid keybase1.UID) error {
	k.log.CDebugf(ctx, "Key family for user %s changed", uid)
	k.setCachedUserInfo(uid, UserInfo{})
	k.clearCachedUnverifiedKeys(uid)

	if k.getCachedCurrentSession().UID == uid {
		// Ignore any errors for now, we don't want to block this
		// notification and it's not worth spawning a goroutine for.
		k.config.MDServer().CheckForRekeys(context.Background())
	}

	return nil
}

// PaperKeyCached implements keybase1.NotifyPaperKeyInterface.
func (k *KeybaseServiceBase) PaperKeyCached(ctx context.Context,
	arg keybase1.PaperKeyCachedArg) error {
	k.log.CDebugf(ctx, "Paper key for %s cached", arg.Uid)

	if k.getCachedCurrentSession().UID == arg.Uid {
		// Ignore any errors for now, we don't want to block this
		// notification and it's not worth spawning a goroutine for.
		k.config.MDServer().CheckForRekeys(context.Background())
	}

	return nil
}

// ClientOutOfDate implements keybase1.NotifySessionInterface.
func (k *KeybaseServiceBase) ClientOutOfDate(ctx context.Context,
	arg keybase1.ClientOutOfDateArg) error {
	k.log.CDebugf(ctx, "Client out of date: %v", arg)
	return nil
}

// ConvertIdentifyError converts a errors during identify into KBFS errors
func ConvertIdentifyError(assertion string, err error) error {
	switch err.(type) {
	case libkb.NotFoundError:
		return NoSuchUserError{assertion}
	case libkb.ResolutionError:
		return NoSuchUserError{assertion}
	}
	return err
}

// Resolve implements the KeybaseService interface for KeybaseServiceBase.
func (k *KeybaseServiceBase) Resolve(ctx context.Context, assertion string) (
	libkb.NormalizedUsername, keybase1.UID, error) {
	user, err := k.identifyClient.Resolve2(ctx, assertion)
	if err != nil {
		return libkb.NormalizedUsername(""), keybase1.UID(""),
			ConvertIdentifyError(assertion, err)
	}
	return libkb.NewNormalizedUsername(user.Username), user.Uid, nil
}

// Identify implements the KeybaseService interface for KeybaseServiceBase.
func (k *KeybaseServiceBase) Identify(ctx context.Context, assertion, reason string) (
	UserInfo, error) {
	// setting UseDelegateUI to true here will cause daemon to use
	// registered identify ui providers instead of terminal if any
	// are available.  If not, then it will use the terminal UI.
	arg := keybase1.Identify2Arg{
		UserAssertion: assertion,
		UseDelegateUI: true,
		Reason:        keybase1.IdentifyReason{Reason: reason},
		// No need to go back and forth with the UI until the service
		// knows for sure there's a need for a dialogue.
		CanSuppressUI: true,
	}

	ei := getExtendedIdentify(ctx)
	if ei.behavior.WarningInsteadOfErrorOnBrokenTracks() {
		arg.ChatGUIMode = true
	}

	res, err := k.identifyClient.Identify2(ctx, arg)
	// Identify2 still returns keybase1.UserPlusKeys data (sans keys),
	// even if it gives a NoSigChainError, and in KBFS it's fine if
	// the user doesn't have a full sigchain yet (e.g., it's just like
	// the sharing before signup case, except the user already has a
	// UID).
	if _, ok := err.(libkb.NoSigChainError); ok {
		k.log.CDebugf(ctx, "Ignoring error (%s) for user %s with no sigchain",
			err, res.Upk.Username)
	} else if err != nil {
		return UserInfo{}, ConvertIdentifyError(assertion, err)
	}

	userInfo, err := k.processUserPlusKeys(res.Upk)
	if err != nil {
		return UserInfo{}, err
	}

	// This is required for every identify call. The userBreak function will take
	// care of checking if res.TrackBreaks is nil or not.
	ei.userBreak(userInfo.Name, userInfo.UID, res.TrackBreaks)

	return userInfo, nil
}

// LoadUserPlusKeys implements the KeybaseService interface for KeybaseServiceBase.
func (k *KeybaseServiceBase) LoadUserPlusKeys(ctx context.Context, uid keybase1.UID) (
	UserInfo, error) {
	cachedUserInfo := k.getCachedUserInfo(uid)
	if cachedUserInfo.Name != libkb.NormalizedUsername("") {
		return cachedUserInfo, nil
	}

	arg := keybase1.LoadUserPlusKeysArg{Uid: uid}
	res, err := k.userClient.LoadUserPlusKeys(ctx, arg)
	if err != nil {
		return UserInfo{}, err
	}

	return k.processUserPlusKeys(res)
}

func (k *KeybaseServiceBase) processUserPlusKeys(upk keybase1.UserPlusKeys) (
	UserInfo, error) {
	verifyingKeys, cryptPublicKeys, kidNames, err := filterKeys(upk.DeviceKeys)
	if err != nil {
		return UserInfo{}, err
	}

	revokedVerifyingKeys, revokedCryptPublicKeys, revokedKidNames, err :=
		filterRevokedKeys(upk.RevokedDeviceKeys)
	if err != nil {
		return UserInfo{}, err
	}

	if len(revokedKidNames) > 0 {
		for k, v := range revokedKidNames {
			kidNames[k] = v
		}
	}

	u := UserInfo{
		Name:                   libkb.NewNormalizedUsername(upk.Username),
		UID:                    upk.Uid,
		VerifyingKeys:          verifyingKeys,
		CryptPublicKeys:        cryptPublicKeys,
		KIDNames:               kidNames,
		RevokedVerifyingKeys:   revokedVerifyingKeys,
		RevokedCryptPublicKeys: revokedCryptPublicKeys,
	}

	k.setCachedUserInfo(upk.Uid, u)
	return u, nil
}

// LoadUnverifiedKeys implements the KeybaseService interface for KeybaseServiceBase.
func (k *KeybaseServiceBase) LoadUnverifiedKeys(ctx context.Context, uid keybase1.UID) (
	[]keybase1.PublicKey, error) {
	if keys, ok := k.getCachedUnverifiedKeys(uid); ok {
		return keys, nil
	}

	arg := keybase1.LoadAllPublicKeysUnverifiedArg{Uid: uid}
	keys, err := k.userClient.LoadAllPublicKeysUnverified(ctx, arg)
	if err != nil {
		return nil, err
	}

	k.setCachedUnverifiedKeys(uid, keys)
	return keys, nil
}

// CurrentSession implements the KeybaseService interface for KeybaseServiceBase.
func (k *KeybaseServiceBase) CurrentSession(ctx context.Context, sessionID int) (
	SessionInfo, error) {
	cachedCurrentSession := k.getCachedCurrentSession()
	if cachedCurrentSession != (SessionInfo{}) {
		return cachedCurrentSession, nil
	}

	res, err := k.sessionClient.CurrentSession(ctx, sessionID)
	if err != nil {
		if ncs := (NoCurrentSessionError{}); err.Error() ==
			NoCurrentSessionExpectedError {
			// Use an error with a proper OS error code attached to
			// it.  TODO: move ErrNoSession from client/go/service to
			// client/go/libkb, so we can use types for the check
			// above.
			err = ncs
		}
		return SessionInfo{}, err
	}
	s, err := SessionInfoFromProtocol(res)
	if err != nil {
		return s, err
	}

	k.log.CDebugf(
		ctx, "new session with username %s, uid %s, crypt public key %s, and verifying key %s",
		s.Name, s.UID, s.CryptPublicKey, s.VerifyingKey)

	k.setCachedCurrentSession(s)

	return s, nil
}

// FavoriteAdd implements the KeybaseService interface for KeybaseServiceBase.
func (k *KeybaseServiceBase) FavoriteAdd(ctx context.Context, folder keybase1.Folder) error {
	return k.favoriteClient.FavoriteAdd(ctx, keybase1.FavoriteAddArg{Folder: folder})
}

// FavoriteDelete implements the KeybaseService interface for KeybaseServiceBase.
func (k *KeybaseServiceBase) FavoriteDelete(ctx context.Context, folder keybase1.Folder) error {
	return k.favoriteClient.FavoriteIgnore(ctx,
		keybase1.FavoriteIgnoreArg{Folder: folder})
}

// FavoriteList implements the KeybaseService interface for KeybaseServiceBase.
func (k *KeybaseServiceBase) FavoriteList(ctx context.Context, sessionID int) ([]keybase1.Folder, error) {
	results, err := k.favoriteClient.GetFavorites(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return results.FavoriteFolders, nil
}

// Notify implements the KeybaseService interface for KeybaseServiceBase.
func (k *KeybaseServiceBase) Notify(ctx context.Context, notification *keybase1.FSNotification) error {
	// Reduce log spam by not repeating log lines for
	// notifications with the same filename.
	//
	// TODO: Only do this in debug mode.
	func() {
		k.lastNotificationFilenameLock.Lock()
		defer k.lastNotificationFilenameLock.Unlock()
		if notification.Filename != k.lastNotificationFilename {
			k.lastNotificationFilename = notification.Filename
			k.log.CDebugf(ctx, "Sending notification for %s", notification.Filename)
		}
	}()
	return k.kbfsClient.FSEvent(ctx, *notification)
}

// NotifySyncStatus implements the KeybaseService interface for
// KeybaseServiceBase.
func (k *KeybaseServiceBase) NotifySyncStatus(ctx context.Context,
	status *keybase1.FSPathSyncStatus) error {
	// Reduce log spam by not repeating log lines for
	// notifications with the same pathname.
	//
	// TODO: Only do this in debug mode.
	func() {
		k.lastNotificationFilenameLock.Lock()
		defer k.lastNotificationFilenameLock.Unlock()
		if status.Path != k.lastSyncNotificationPath {
			k.lastSyncNotificationPath = status.Path
			k.log.CDebugf(ctx, "Sending notification for %s", status.Path)
		}
	}()
	return k.kbfsClient.FSSyncEvent(ctx, *status)
}

// FlushUserFromLocalCache implements the KeybaseService interface for
// KeybaseServiceBase.
func (k *KeybaseServiceBase) FlushUserFromLocalCache(ctx context.Context,
	uid keybase1.UID) {
	k.log.CDebugf(ctx, "Flushing cache for user %s", uid)
	k.setCachedUserInfo(uid, UserInfo{})
}

// FlushUserUnverifiedKeysFromLocalCache implements the KeybaseService interface for
// KeybaseServiceBase.
func (k *KeybaseServiceBase) FlushUserUnverifiedKeysFromLocalCache(ctx context.Context,
	uid keybase1.UID) {
	k.log.CDebugf(ctx, "Flushing cache of unverified keys for user %s", uid)
	k.clearCachedUnverifiedKeys(uid)
}

// CtxKeybaseServiceTagKey is the type used for unique context tags
// used while servicing incoming keybase requests.
type CtxKeybaseServiceTagKey int

const (
	// CtxKeybaseServiceIDKey is the type of the tag for unique
	// operation IDs used while servicing incoming keybase requests.
	CtxKeybaseServiceIDKey CtxKeybaseServiceTagKey = iota
)

// CtxKeybaseServiceOpID is the display name for the unique operation
// enqueued rekey ID tag.
const CtxKeybaseServiceOpID = "KSID"

func (k *KeybaseServiceBase) getHandleFromFolderName(ctx context.Context,
	tlfName string, public bool) (*TlfHandle, error) {
	for {
		tlfHandle, err := ParseTlfHandle(ctx, k.config.KBPKI(), tlfName, public)
		switch e := err.(type) {
		case TlfNameNotCanonical:
			tlfName = e.NameToTry
		case nil:
			return tlfHandle, nil
		default:
			return nil, err
		}
	}
}

// FSEditListRequest implements keybase1.NotifyFSRequestInterface for
// KeybaseServiceBase.
func (k *KeybaseServiceBase) FSEditListRequest(ctx context.Context,
	req keybase1.FSEditListRequest) (err error) {
	ctx = ctxWithRandomIDReplayable(ctx, CtxKeybaseServiceIDKey, CtxKeybaseServiceOpID,
		k.log)
	k.log.CDebugf(ctx, "Edit list request for %s (public: %t)",
		req.Folder.Name, !req.Folder.Private)
	tlfHandle, err := k.getHandleFromFolderName(ctx, req.Folder.Name,
		!req.Folder.Private)
	if err != nil {
		return err
	}

	rootNode, _, err := k.config.KBFSOps().
		GetOrCreateRootNode(ctx, tlfHandle, MasterBranch)
	if err != nil {
		return err
	}
	editHistory, err := k.config.KBFSOps().GetEditHistory(ctx,
		rootNode.GetFolderBranch())
	if err != nil {
		return err
	}

	// Convert the edits to an RPC response.
	var resp keybase1.FSEditListArg
	for writer, edits := range editHistory {
		for _, edit := range edits {
			var nType keybase1.FSNotificationType
			switch edit.Type {
			case FileCreated:
				nType = keybase1.FSNotificationType_FILE_CREATED
			case FileModified:
				nType = keybase1.FSNotificationType_FILE_MODIFIED
			default:
				k.log.CDebugf(ctx, "Bad notification type in edit history: %v",
					edit.Type)
				continue
			}
			n := keybase1.FSNotification{
				PublicTopLevelFolder: !req.Folder.Private,
				Filename:             edit.Filepath,
				StatusCode:           keybase1.FSStatusCode_FINISH,
				NotificationType:     nType,
				WriterUid:            writer,
				LocalTime:            keybase1.ToTime(edit.LocalTime),
			}
			resp.Edits = append(resp.Edits, n)
		}
	}
	resp.RequestID = req.RequestID

	k.log.CDebugf(ctx, "Sending edit history response with %d edits",
		len(resp.Edits))
	return k.kbfsClient.FSEditList(ctx, resp)
}

// FSSyncStatusRequest implements keybase1.NotifyFSRequestInterface for
// KeybaseServiceBase.
func (k *KeybaseServiceBase) FSSyncStatusRequest(ctx context.Context,
	req keybase1.FSSyncStatusRequest) (err error) {
	k.log.CDebugf(ctx, "Got sync status request: %v", req)

	resp := keybase1.FSSyncStatusArg{RequestID: req.RequestID}

	// For now, just return the number of syncing bytes.
	jServer, err := GetJournalServer(k.config)
	if err == nil {
		status, _ := jServer.Status(ctx)
		resp.Status.TotalSyncingBytes = status.UnflushedBytes
		k.log.CDebugf(ctx, "Sending sync status response with %d syncing bytes",
			status.UnflushedBytes)
	} else {
		k.log.CDebugf(ctx, "No journal server, sending empty response")
	}

	return k.kbfsClient.FSSyncStatus(ctx, resp)
}

// GetTLFCryptKeys implements the TlfKeysInterface interface for
// KeybaseServiceBase.
func (k *KeybaseServiceBase) GetTLFCryptKeys(ctx context.Context,
	query keybase1.TLFQuery) (res keybase1.GetTLFCryptKeysRes, err error) {
	if ctx, err = makeExtendedIdentify(
		ctxWithRandomIDReplayable(ctx,
			CtxKeybaseServiceIDKey, CtxKeybaseServiceOpID, k.log),
		query.IdentifyBehavior,
	); err != nil {
		return keybase1.GetTLFCryptKeysRes{}, err
	}

	tlfHandle, err := k.getHandleFromFolderName(ctx, query.TlfName, false)
	if err != nil {
		return res, err
	}

	res.NameIDBreaks.CanonicalName = keybase1.CanonicalTlfName(
		tlfHandle.GetCanonicalName())

	keys, id, err := k.config.KBFSOps().GetTLFCryptKeys(ctx, tlfHandle)
	if err != nil {
		return res, err
	}
	res.NameIDBreaks.TlfID = keybase1.TLFID(id.String())

	for i, key := range keys {
		res.CryptKeys = append(res.CryptKeys, keybase1.CryptKey{
			KeyGeneration: int(FirstValidKeyGen) + i,
			Key:           keybase1.Bytes32(key.Data()),
		})
	}

	if query.IdentifyBehavior.WarningInsteadOfErrorOnBrokenTracks() {
		res.NameIDBreaks.Breaks = getExtendedIdentify(ctx).getTlfBreakAndClose()
	}

	return res, nil
}

// GetPublicCanonicalTLFNameAndID implements the TlfKeysInterface interface for
// KeybaseServiceBase.
func (k *KeybaseServiceBase) GetPublicCanonicalTLFNameAndID(
	ctx context.Context, query keybase1.TLFQuery) (
	res keybase1.CanonicalTLFNameAndIDWithBreaks, err error) {
	if ctx, err = makeExtendedIdentify(
		ctxWithRandomIDReplayable(ctx,
			CtxKeybaseServiceIDKey, CtxKeybaseServiceOpID, k.log),
		query.IdentifyBehavior,
	); err != nil {
		return keybase1.CanonicalTLFNameAndIDWithBreaks{}, err
	}

	tlfHandle, err := k.getHandleFromFolderName(
		ctx, query.TlfName, true /* public */)
	if err != nil {
		return res, err
	}

	res.CanonicalName = keybase1.CanonicalTlfName(
		tlfHandle.GetCanonicalName())

	id, err := k.config.KBFSOps().GetTLFID(ctx, tlfHandle)
	if err != nil {
		return res, err
	}
	res.TlfID = keybase1.TLFID(id.String())

	if query.IdentifyBehavior.WarningInsteadOfErrorOnBrokenTracks() {
		res.Breaks = getExtendedIdentify(ctx).getTlfBreakAndClose()
	}

	return res, nil
}

// EstablishMountDir asks the service for the current mount path
func (k *KeybaseServiceBase) EstablishMountDir(ctx context.Context) (
	string, error) {
	dir, err := k.kbfsMountClient.GetCurrentMountDir(ctx)
	if err != nil {
		return "", err
	}
	if dir == "" {
		dirs, err2 := k.kbfsMountClient.GetAllAvailableMountDirs(ctx)
		if err != nil {
			return "", err2
		}
		dir, err = chooseDefaultMount(ctx, dirs, k.log)
		if err != nil {
			return "", err
		}
		err2 = k.kbfsMountClient.SetCurrentMountDir(ctx, dir)
		if err2 != nil {
			k.log.CInfof(ctx, "SetCurrentMount Dir fails - ", err2)
		}
		// Continue mounting even if we can't save the mount
		k.log.CDebugf(ctx, "Choosing mountdir %s from %v", dir, dirs)
	}
	return dir, err
}
