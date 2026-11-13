export interface StoredFile {
    id: number;
    parent_id: number | null;
    is_folder: boolean;
    name: string;
    content_type: string;
    size_bytes: number;
    created_at: string;
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

export interface MoveForm {
    ids: number[];
    parent_id: number | null;
}

export interface PathEntry {
    id: number;
    name: string;
}

// A folder and its parents, root first. The root itself is not in it.
export interface FolderPath {
    path: PathEntry[];
}
