export interface User {
    id?: number;
    display_name: string;
    email: string;
    email_confirmed?: Date | null;
    role_id?: number;
    role?: Role;
    deleted_at?: Date | null;
}

export interface Invitation {
    id?: number;
    email: string;
    role_id?: number;
    role?: Role;
    created_at?: Date;
}

export interface Role {
    id?: number;
    name: string;
    description?: string | null;
    is_default: boolean;
    permissions?: Permission[];
}

export interface Permission {
    id: number;
    name: string;
    description: string | null;
}

// The user's own settings for the file lists and how times show. The server keeps them, so every device shares them.
export interface Settings {
    sort_favorites_first: boolean;
    sort_folders_first: boolean;
    show_hidden: boolean;
    show_extensions: boolean;
    use_24_hour_clock: boolean;
}
