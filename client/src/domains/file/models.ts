export interface StoredFile {
    id: number;
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

export interface PaginatedFiles {
    data: StoredFile[];
    page: number;
    page_size: number;
    total: number;
    total_pages: number;
}
