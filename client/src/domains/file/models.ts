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
