// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libfuse

import (
	"os"
	"runtime"
	"strings"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/logger"
	"github.com/keybase/kbfs/libfs"
	"github.com/keybase/kbfs/libkbfs"
	"golang.org/x/net/context"
)

// FS implements the newfuse FS interface for KBFS.
type FS struct {
	config libkbfs.Config
	fuse   *fs.Server
	conn   *fuse.Conn
	log    logger.Logger
	errLog logger.Logger

	notifications *libfs.FSNotifications

	// remoteStatus is the current status of remote connections.
	remoteStatus libfs.RemoteStatus

	// this is like time.AfterFunc, except that in some tests this can be
	// overridden to execute f without any delay.
	execAfterDelay func(d time.Duration, f func())

	root Root

	platformParams PlatformParams
}

// NewFS creates an FS
func NewFS(config libkbfs.Config, conn *fuse.Conn, debug bool, platformParams PlatformParams) *FS {
	log := config.MakeLogger("kbfsfuse")
	// We need extra depth for errors, so that we can report the line
	// number for the caller of reportErr, not reportErr itself.
	errLog := log.CloneWithAddedDepth(1)
	if debug {
		// Turn on debugging.  TODO: allow a proper log file and
		// style to be specified.
		log.Configure("", true, "")
		errLog.Configure("", true, "")
	}
	fs := &FS{
		config:         config,
		conn:           conn,
		log:            log,
		errLog:         errLog,
		notifications:  libfs.NewFSNotifications(log),
		platformParams: platformParams,
	}
	fs.root.private = &FolderList{
		fs:      fs,
		folders: make(map[string]*TLF),
	}
	fs.root.public = &FolderList{
		fs:      fs,
		public:  true,
		folders: make(map[string]*TLF),
	}
	fs.execAfterDelay = func(d time.Duration, f func()) {
		time.AfterFunc(d, f)
	}
	return fs
}

// SetFuseConn sets fuse connection for this FS.
func (f *FS) SetFuseConn(fuse *fs.Server, conn *fuse.Conn) {
	f.fuse = fuse
	f.conn = conn
}

// NotificationGroupWait - wait on the notification group.
func (f *FS) NotificationGroupWait() {
	f.notifications.Wait()
}

func (f *FS) queueNotification(fn func()) {
	f.notifications.QueueNotification(fn)
}

// LaunchNotificationProcessor launches the notification processor.
func (f *FS) LaunchNotificationProcessor(ctx context.Context) {
	f.notifications.LaunchProcessor(ctx)
}

// WithContext adds app- and request-specific values to the context.
// libkbfs.NewContextWithCancellationDelayer is called before returning the
// context to ensure the cancellation is controllable.
//
// It is called by FUSE for normal runs, but may be called explicitly in other
// settings, such as tests.
func (f *FS) WithContext(ctx context.Context) context.Context {
	id, errRandomReqID := libkbfs.MakeRandomRequestID()
	if errRandomReqID != nil {
		f.log.Errorf("Couldn't make request ID: %v", errRandomReqID)
	}

	// context.WithDeadline uses clock from `time` package, so we are not using
	// f.config.Clock() here
	start := time.Now()
	ctx, err := libkbfs.NewContextWithCancellationDelayer(
		libkbfs.NewContextReplayable(ctx, func(ctx context.Context) context.Context {
			ctx = context.WithValue(ctx, libfs.CtxAppIDKey, f)
			logTags := make(logger.CtxLogTags)
			logTags[CtxIDKey] = CtxOpID
			ctx = logger.NewContextWithLogTags(ctx, logTags)

			if errRandomReqID == nil {
				// Add a unique ID to this context, identifying a particular
				// request.
				ctx = context.WithValue(ctx, CtxIDKey, id)
			}

			if runtime.GOOS == "darwin" {
				// Timeout operations before they hit the osxfuse time limit,
				// so we don't hose the entire mount (Fixed in OSXFUSE 3.2.0).
				// The timeout is 60 seconds, but it looks like sometimes it
				// tries multiple attempts within that 60 seconds, so let's go
				// a little under 60/3 to be safe.
				//
				// It should be safe to ignore the CancelFunc here because our
				// parent context will be canceled by the FUSE serve loop.
				ctx, _ = context.WithDeadline(ctx, start.Add(19*time.Second))
			}

			return ctx

		}))

	if err != nil {
		panic(err) // this should never happen
	}

	return ctx
}

// Serve FS. Will block.
func (f *FS) Serve(ctx context.Context) error {
	srv := fs.New(f.conn, &fs.Config{
		WithContext: func(ctx context.Context, _ fuse.Request) context.Context {
			return f.WithContext(ctx)
		},
	})
	f.fuse = srv

	f.notifications.LaunchProcessor(ctx)
	f.remoteStatus.Init(ctx, f.log, f.config, f)
	// Blocks forever, unless an interrupt signal is received
	// (handled by libkbfs.Init).
	return srv.Serve(f)
}

// UserChanged is called from libfs.
func (f *FS) UserChanged(ctx context.Context, oldName, newName libkb.NormalizedUsername) {
	f.log.CDebugf(ctx, "User changed: %q -> %q", oldName, newName)
	f.root.public.userChanged(ctx, oldName, newName)
	f.root.private.userChanged(ctx, oldName, newName)
}

var _ libfs.RemoteStatusUpdater = (*FS)(nil)

var _ fs.FS = (*FS)(nil)

var _ fs.FSStatfser = (*FS)(nil)

func (f *FS) reportErr(ctx context.Context,
	mode libkbfs.ErrorModeType, err error) {
	if err == nil {
		f.errLog.CDebugf(ctx, "Request complete")
		return
	}

	f.config.Reporter().ReportErr(ctx, "", false, mode, err)
	// We just log the error as debug, rather than error, because it
	// might just indicate an expected error such as an ENOENT.
	//
	// TODO: Classify errors and escalate the logging level of the
	// important ones.
	f.errLog.CDebugf(ctx, err.Error())
}

// Root implements the fs.FS interface for FS.
func (f *FS) Root() (fs.Node, error) {
	return &f.root, nil
}

// Statfs implements the fs.FSStatfser interface for FS.
func (f *FS) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) error {
	// TODO: Fill in real values for these.
	var bsize uint32 = 32 * 1024
	*resp = fuse.StatfsResponse{
		Blocks:  ^uint64(0) / uint64(bsize),
		Bfree:   ^uint64(0) / uint64(bsize),
		Bavail:  ^uint64(0) / uint64(bsize),
		Files:   0,
		Ffree:   0,
		Bsize:   bsize,
		Namelen: ^uint32(0),
		Frsize:  0,
	}
	return nil
}

// Root represents the root of the KBFS file system.
type Root struct {
	private *FolderList
	public  *FolderList
}

var _ fs.NodeAccesser = (*FolderList)(nil)

// Access implements fs.NodeAccesser interface for *Root.
func (*Root) Access(ctx context.Context, r *fuse.AccessRequest) error {
	if int(r.Uid) != os.Getuid() {
		// short path: not accessible by anybody other than the logged in user.
		// This is in case we enable AllowOther in the future.
		return fuse.EPERM
	}

	if r.Mask&02 != 0 {
		return fuse.EPERM
	}

	return nil
}

var _ fs.Node = (*Root)(nil)

// Attr implements the fs.Node interface for Root.
func (*Root) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0500
	return nil
}

var _ fs.NodeRequestLookuper = (*Root)(nil)

// Lookup implements the fs.NodeRequestLookuper interface for Root.
func (r *Root) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (_ fs.Node, err error) {
	r.log().CDebugf(ctx, "FS Lookup %s", req.Name)
	defer func() { r.private.fs.reportErr(ctx, libkbfs.ReadMode, err) }()

	specialNode := handleNonTLFSpecialFile(
		req.Name, r.private.fs, &resp.EntryValid)
	if specialNode != nil {
		return specialNode, nil
	}

	platformNode, err := r.platformLookup(ctx, req, resp)
	if platformNode != nil || err != nil {
		return platformNode, err
	}

	switch req.Name {
	case PrivateName:
		return r.private, nil
	case PublicName:
		return r.public, nil
	}

	// Don't want to pop up errors on special OS files.
	if strings.HasPrefix(req.Name, ".") {
		return nil, fuse.ENOENT
	}

	return nil, libkbfs.NoSuchFolderListError{
		Name:     req.Name,
		PrivName: PrivateName,
		PubName:  PublicName,
	}
}

// PathType returns PathType for this folder
func (r *Root) PathType() libkbfs.PathType {
	return libkbfs.KeybasePathType
}

var _ fs.NodeCreater = (*Root)(nil)

// Create implements the fs.NodeCreater interface for Root.
func (r *Root) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (_ fs.Node, _ fs.Handle, err error) {
	r.log().CDebugf(ctx, "FS Create")
	defer func() { r.private.fs.reportErr(ctx, libkbfs.WriteMode, err) }()
	return nil, nil, libkbfs.NewWriteUnsupportedError(libkbfs.BuildCanonicalPath(r.PathType(), req.Name))
}

// Mkdir implements the fs.NodeMkdirer interface for Root.
func (r *Root) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (_ fs.Node, err error) {
	r.log().CDebugf(ctx, "FS Mkdir")
	defer func() { r.private.fs.reportErr(ctx, libkbfs.WriteMode, err) }()
	return nil, libkbfs.NewWriteUnsupportedError(libkbfs.BuildCanonicalPath(r.PathType(), req.Name))
}

var _ fs.Handle = (*Root)(nil)

var _ fs.HandleReadDirAller = (*Root)(nil)

// ReadDirAll implements the ReadDirAll interface for Root.
func (r *Root) ReadDirAll(ctx context.Context) (res []fuse.Dirent, err error) {
	r.log().CDebugf(ctx, "FS ReadDirAll")
	defer func() { r.private.fs.reportErr(ctx, libkbfs.ReadMode, err) }()
	res = []fuse.Dirent{
		{
			Type: fuse.DT_Dir,
			Name: PrivateName,
		},
		{
			Type: fuse.DT_Dir,
			Name: PublicName,
		},
	}
	if r.private.fs.platformParams.shouldAppendPlatformRootDirs() {
		res = append(res, platformRootDirs...)
	}

	if name := r.private.fs.remoteStatus.ExtraFileName(); name != "" {
		res = append(res, fuse.Dirent{Type: fuse.DT_File, Name: name})
	}
	return res, nil
}

func (r *Root) log() logger.Logger {
	return r.private.fs.log
}
