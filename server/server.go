package server

import (
	"errors"
	"os"
	"path"
	"sync"
	"time"

	"golang.org/x/net/context"

	"github.com/coreos/agro"
	"github.com/coreos/agro/blockset"
	"github.com/coreos/agro/models"
	"github.com/coreos/agro/server/gc"
	"github.com/RoaringBitmap/roaring"

	// Register drivers
	_ "github.com/coreos/agro/metadata/memory"
	_ "github.com/coreos/agro/storage/block"
)

var _ agro.Server = &server{}

type server struct {
	mut           sync.RWMutex
	writeableLock sync.RWMutex
	blocks        agro.BlockStore
	mds           agro.MetadataService
	inodes        *INodeStore
	peersMap      map[string]*models.PeerInfo
	closeChans    []chan interface{}
	openINodeRefs map[string]map[agro.INodeID]int
	openFiles     []*file
	cfg           agro.Config
	peerInfo      *models.PeerInfo

	internalAddr string

	heartbeating    bool
	replicationOpen bool
	gc              gc.GC
}

func (s *server) inodeRefForPath(p agro.Path) (agro.INodeRef, error) {
	dirname, filename := path.Split(p.Path)
	dirpath := agro.Path{p.Volume, dirname}
	dir, _, err := s.mds.Getdir(dirpath)
	if err != nil {
		return agro.INodeRef{}, err
	}

	volID, err := s.mds.GetVolumeID(p.Volume)
	if err != nil {
		return agro.INodeRef{}, err
	}

	inodeID, ok := dir.Files[filename]
	if !ok {
		return agro.INodeRef{}, os.ErrNotExist
	}

	return agro.NewINodeRef(volID, agro.INodeID(inodeID)), nil
}

type FileInfo struct {
	INode *models.INode
	Path  agro.Path
	Ref   agro.INodeRef
}

func (fi FileInfo) Name() string {
	return fi.Path.Path
}

func (fi FileInfo) Size() int64 {
	return int64(fi.INode.Filesize)
}

func (fi FileInfo) Mode() os.FileMode {
	return os.FileMode(fi.INode.Permissions.Mode)
}

func (fi FileInfo) ModTime() time.Time {
	return time.Unix(0, int64(fi.INode.Permissions.Mtime))
}

func (fi FileInfo) IsDir() bool {
	return fi.Path.IsDir()
}

func (fi FileInfo) Sys() interface{} {
	return fi
}

func (s *server) Lstat(path agro.Path) (os.FileInfo, error) {
	s.mut.RLock()
	defer s.mut.RUnlock()
	clog.Debugf("lstat %s", path)
	for _, x := range s.openFiles {
		if x.path.Equals(path) {
			return x.Stat()
		}
	}
	ref, err := s.inodeRefForPath(path)
	if err != nil {
		return nil, err
	}

	inode, err := s.inodes.GetINode(context.TODO(), ref)
	if err != nil {
		return nil, err
	}

	return FileInfo{inode, path, ref}, nil
}

func (s *server) Readdir(path agro.Path) ([]agro.Path, error) {
	if !path.IsDir() {
		return nil, errors.New("ENOTDIR")
	}

	dir, subdirs, err := s.mds.Getdir(path)
	if err != nil {
		return nil, err
	}

	var entries []agro.Path
	entries = append(entries, subdirs...)

	for filename := range dir.Files {
		childPath, ok := path.Child(filename)
		if !ok {
			return nil, errors.New("server: entry path is not a directory")
		}

		entries = append(entries, childPath)
	}

	return entries, nil
}

func (s *server) Rename(from, to agro.Path) error {
	//TODO(barakmich): Handle hard links
	ref, err := s.inodeRefForPath(from)
	if err != nil {
		return err
	}
	clog.Debugf("renaming %s %s %#v", from, to, ref)
	_, err = s.mds.SetFileINode(from, agro.NewINodeRef(0, 0))
	if err != nil {
		return err
	}
	_, err = s.mds.SetFileINode(to, ref)
	if err != nil {
		return err
	}
	return nil
}

func (s *server) Mkdir(path agro.Path) error {
	if !path.IsDir() {
		return os.ErrInvalid
	}
	return s.mds.Mkdir(path, &models.Metadata{})
}
func (s *server) CreateVolume(vol string) error {
	err := s.mds.CreateVolume(vol)
	if err == agro.ErrAgain {
		return s.CreateVolume(vol)
	}
	return err
}

func (s *server) GetVolumes() ([]string, error) {
	return s.mds.GetVolumes()
}

func (s *server) Close() error {
	for _, c := range s.closeChans {
		close(c)
	}
	if s.gc != nil {
		s.gc.Stop()
	}
	err := s.mds.Close()
	if err != nil {
		clog.Errorf("couldn't close mds: %s", err)
		return err
	}
	err = s.inodes.Close()
	if err != nil {
		clog.Errorf("couldn't close inodes: %s", err)
		return err
	}
	err = s.blocks.Close()
	if err != nil {
		clog.Errorf("couldn't close blocks: %s", err)
		return err
	}
	return nil
}

func (s *server) incRef(vol string, bm *roaring.Bitmap) {
	s.mut.Lock()
	defer s.mut.Unlock()
	if bm.GetCardinality() == 0 {
		return
	}
	if _, ok := s.openINodeRefs[vol]; !ok {
		s.openINodeRefs[vol] = make(map[agro.INodeID]int)
	}
	it := bm.Iterator()
	for it.HasNext() {
		id := agro.INodeID(it.Next())
		v, ok := s.openINodeRefs[vol][id]
		if !ok {
			s.openINodeRefs[vol][id] = 1
		} else {
			s.openINodeRefs[vol][id] = v + 1
		}
	}
}

func (s *server) decRef(vol string, bm *roaring.Bitmap) {
	s.mut.Lock()
	defer s.mut.Unlock()
	it := bm.Iterator()
	for it.HasNext() {
		id := agro.INodeID(it.Next())
		v, ok := s.openINodeRefs[vol][id]
		if !ok {
			panic("server: double remove of an inode reference")
		} else {
			v--
			if v == 0 {
				delete(s.openINodeRefs[vol], id)
			} else {
				s.openINodeRefs[vol][id] = v
			}
		}
	}
	if len(s.openINodeRefs[vol]) == 0 {
		delete(s.openINodeRefs, vol)
	}
}

func (s *server) getBitmap(vol string) (*roaring.Bitmap, bool) {
	s.mut.Lock()
	defer s.mut.Unlock()
	if _, ok := s.openINodeRefs[vol]; !ok {
		return nil, false
	}
	out := roaring.NewBitmap()
	for k := range s.openINodeRefs[vol] {
		out.Add(uint32(k))
	}
	return out, true
}

func (s *server) Remove(path agro.Path) error {
	if path.IsDir() {
		return s.removeDir(path)
	}
	return s.removeFile(path)
}

func (s *server) removeFile(p agro.Path) error {
	ref, err := s.inodeRefForPath(p)
	if err != nil {
		return err
	}
	inode, err := s.inodes.GetINode(context.TODO(), ref)
	if err != nil {
		return err
	}
	bs, err := blockset.UnmarshalFromProto(inode.Blocks, s.blocks)
	if err != nil {
		return err
	}
	live := bs.GetLiveINodes()
	_, err = s.mds.SetFileINode(p, agro.NewINodeRef(ref.Volume(), 0))
	if err != nil {
		return err
	}
	// Anybody who had it open still does, and a write/sync will bring it back,
	// as expected. So this is safe to modify.
	return s.mds.ModifyDeadMap(p.Volume, roaring.NewBitmap(), live)
}

func (s *server) removeDir(path agro.Path) error {
	return s.mds.Rmdir(path)
}

func (s *server) addOpenFile(f *file) {
	s.mut.Lock()
	defer s.mut.Unlock()
	s.openFiles = append(s.openFiles, f)
}

func (s *server) removeOpenFile(f *file) {
	s.mut.Lock()
	defer s.mut.Unlock()
	for i, x := range s.openFiles {
		if x == f {
			s.openFiles = append(s.openFiles[:i], s.openFiles[i+1:]...)
			return
		}
	}
}
