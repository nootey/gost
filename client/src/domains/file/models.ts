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

export interface PaginatedFiles {
    data: StoredFile[];
    page: number;
    page_size: number;
    total: number;
    total_pages: number;
}

export interface FolderForm {
    name: string;
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
