package store

import (
	"slices"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// ResourceOwner identifies the tenant, app, and optional user of a resource.
type ResourceOwner struct {
	TenantKey, AppID, UserOpenID string
}

// ResourceOwner resolves an app identity and checks its optional user's current membership.
func (s *Store) ResourceOwner(appID, userOpenID string) (ResourceOwner, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	app, ok := s.apps.items[appID]
	if !ok {
		return ResourceOwner{}, false
	}
	if userOpenID != "" {
		user, ok := s.users.items[userOpenID]
		if !ok || user.Inactive || user.TenantKey != app.TenantKey {
			return ResourceOwner{}, false
		}
	}
	return ResourceOwner{TenantKey: app.TenantKey, AppID: appID, UserOpenID: userOpenID}, true
}

// Document stores the title and editable content of one document.
type Document struct {
	Owner                      ResourceOwner
	ID, Title, Content, Format string
	Revision                   int64
}

// WikiSpace holds nodes owned by one sandbox identity.
type WikiSpace struct {
	Owner                 ResourceOwner
	ID, Name, Description string
}

// WikiNode links a space entry to its document or file.
type WikiNode struct {
	ID, SpaceID, ParentID, ObjectID, ObjectType, Kind, Title string
	HasChild                                                 bool
}

// DriveFile stores uploaded bytes and their Drive metadata.
type DriveFile struct {
	Owner                         ResourceOwner
	ID, Parent, Name, ContentType string
	Data                          []byte
}

// DriveRoot is the virtual root folder of the requesting identity.
const DriveRoot = "fld_larkbox_root"

type contentState struct {
	docs   map[string]Document
	spaces map[string]WikiSpace
	nodes  map[string]WikiNode
	files  map[string]DriveFile
	sheets map[string]Spreadsheet
	bases  map[string]Base
}

func newContent() contentState {
	return contentState{
		docs: make(map[string]Document), spaces: make(map[string]WikiSpace),
		nodes: make(map[string]WikiNode), files: make(map[string]DriveFile), sheets: make(map[string]Spreadsheet), bases: make(map[string]Base),
	}
}

// CreateDocument saves an authored document with its first revision.
func (s *Store) CreateDocument(owner ResourceOwner, title, body, format string) Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createDocument(owner, title, body, format)
}

// createDocument requires the store lock.
func (s *Store) createDocument(owner ResourceOwner, title, body, format string) Document {
	entry := Document{Owner: owner, ID: protocol.NewID("docx"), Title: title, Content: body, Format: format, Revision: 1}
	s.content.docs[entry.ID] = entry
	return entry
}

// Document returns a document visible to its owning identity.
func (s *Store) Document(owner ResourceOwner, id string) (Document, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.content.docs[id]
	if !ok || entry.Owner != owner {
		return Document{}, false
	}
	return entry, true
}

// CreateWikiSpace creates an empty wiki space for a caller.
func (s *Store) CreateWikiSpace(owner ResourceOwner, name, description string) WikiSpace {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := WikiSpace{Owner: owner, ID: protocol.NewID(protocol.CardID), Name: name, Description: description}
	s.content.spaces[entry.ID] = entry
	return entry
}

// WikiSpace returns a space visible to the request identity.
func (s *Store) WikiSpace(owner ResourceOwner, id string) (WikiSpace, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	space, ok := s.content.spaces[id]
	if !ok || space.Owner != owner {
		return WikiSpace{}, false
	}
	return space, true
}

// CreateWikiNode adds a document under a visible space and optional parent.
func (s *Store) CreateWikiNode(owner ResourceOwner, spaceID, parentID, title string) (WikiNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	space, found := s.content.spaces[spaceID]
	if !found || space.Owner != owner {
		return WikiNode{}, ErrNotFound
	}
	if parentID != "" {
		parent, ok := s.content.nodes[parentID]
		if !ok || parent.SpaceID != spaceID {
			return WikiNode{}, ErrNotFound
		}
		parent.HasChild = true
		s.content.nodes[parentID] = parent
	}
	doc := s.createDocument(owner, title, "", "markdown")
	node := WikiNode{ID: protocol.NewID("wik"), SpaceID: spaceID, ParentID: parentID,
		ObjectID: doc.ID, ObjectType: "docx", Kind: "origin", Title: title}
	s.content.nodes[node.ID] = node
	return node, nil
}

// WikiNode resolves a node token or its original document token. An empty
// kind accepts either representation.
func (s *Store) WikiNode(owner ResourceOwner, id, kind string) (WikiNode, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if kind == "" || kind == "wiki" {
		node, found := s.content.nodes[id]
		if found && s.content.spaces[node.SpaceID].Owner == owner {
			return node, true
		}
		if kind == "wiki" {
			return WikiNode{}, false
		}
	}
	for _, node := range s.content.nodes {
		if (kind == "" || node.ObjectType == kind) && node.ObjectID == id && s.content.spaces[node.SpaceID].Owner == owner {
			return node, true
		}
	}
	return WikiNode{}, false
}

// UploadDriveFile saves a file in the creator's Drive root.
func (s *Store) UploadDriveFile(entry DriveFile) (DriveFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.Parent != DriveRoot {
		return DriveFile{}, ErrNotFound
	}
	if entry.ID == "" {
		entry.ID = protocol.NewID("box")
	} else if original, found := s.content.files[entry.ID]; !found || original.Owner != entry.Owner {
		return DriveFile{}, ErrNotFound
	}
	entry.Data = slices.Clone(entry.Data)
	s.content.files[entry.ID] = entry
	entry.Data = nil
	return entry, nil
}

// DriveFile returns file metadata and, when requested, a copy of the bytes.
func (s *Store) DriveFile(owner ResourceOwner, id string, includeBytes bool) (DriveFile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.content.files[id]
	if !found || entry.Owner != owner {
		return DriveFile{}, false
	}
	if includeBytes {
		entry.Data = slices.Clone(entry.Data)
	} else {
		entry.Data = nil
	}
	return entry, true
}
