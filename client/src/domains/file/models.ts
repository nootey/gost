export interface StoredFile {
    id: number;
    parent_id: number | null;
    is_folder: boolean;
    name: string;
    content_type: string;
    size_bytes: number;
    created_at: string;
    // True until the copy job ends. Until then the item does not open, and it cannot be selected or get a menu.
    // The list does not poll, so the user reloads the page to see the copy end.
    is_copying: boolean;
    is_favorite: boolean;
}

export interface UploadResponse {
    title: string;
    message: string;
    file: StoredFile;
}

export interface FolderResponse {
    title: string;
    message: string;
    folder: StoredFile;
}

export interface RenameResponse {
    title: string;
    message: string;
    file: StoredFile;
}

export type SortField = "name" | "size" | "type" | "date";

// A null parent_id is the root. A null cursor is the first page. A null search matches every name.
export interface ListQuery {
    parent_id: number | null;
    cursor: string | null;
    page_size: number;
    sort: SortField;
    order: "asc" | "desc";
    search: string | null;
}

// An item the user deleted. parent_id is the folder it was in.
export interface TrashedFile extends StoredFile {
    trashed_at: string;
}

// A null cursor is the first page. The date sort is the time of the delete.
export interface TrashQuery {
    cursor: string | null;
    page_size: number;
    sort: SortField;
    order: "asc" | "desc";
}

export interface FolderForm {
    name: string;
    parent_id: number | null;
}

export interface RenameForm {
    name: string;
}

export interface ItemsForm {
    ids: number[];
}

// Moves or copies the items into one folder. A null parent_id is the root.
export interface PasteForm {
    ids: number[];
    parent_id: number | null;
}

export interface PathEntry {
    id: number;
    name: string;
}

// building until the zip job ends, then ready or failed. error says why a failed zip failed.
export type DownloadState = "building" | "ready" | "failed";

// A zip of a folder or many items. A job builds it, and the downloads list in the header shows it. expires_at is
// when the server removes it. downloaded is true once the user got the ready zip.
export interface Download {
    id: number;
    name: string;
    state: DownloadState;
    error?: string;
    size_bytes: number;
    created_at: string;
    expires_at: string;
    downloaded: boolean;
}

export interface DownloadList {
    downloads: Download[];
}

export interface DownloadResponse {
    title: string;
    message: string;
    download: Download;
}

// A folder and its parents, root first. The root itself is not in it.
export interface FolderPath {
    path: PathEntry[];
}
