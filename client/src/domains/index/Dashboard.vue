<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { useFileStore } from "../../pkg/stores/file_store.ts";
import { useToastStore } from "../../pkg/stores/toast_store.ts";
import UploadDropzone from "../file/components/UploadDropzone.vue";
import FilesPaginated from "../file/components/FilesPaginated.vue";
import FolderBreadcrumb from "../file/components/FolderBreadcrumb.vue";
import NewFolderDialog from "../file/components/NewFolderDialog.vue";
import type { PathEntry } from "../file/models.ts";

const route = useRoute();
const router = useRouter();
const fileStore = useFileStore();
const toastStore = useToastStore();

const filesRef = ref<{ refresh: () => Promise<void> } | null>(null);

const routeFolderId = computed<number | null>(() => (route.name === "folder" ? Number(route.params.id) : null));

// The page switches to a folder only after its path loads, so an unknown folder never shows as an empty one.
const folderId = ref<number | null>(null);
const path = ref<PathEntry[]>([]);
const ready = ref<boolean>(false);
const newFolderVisible = ref<boolean>(false);

const folderName = computed<string | undefined>(() => path.value.at(-1)?.name);

// Only the newest request may switch the folder, so a slow old one cannot undo a later click.
let latestRequest = 0;

async function openFolder(id: number | null): Promise<void> {
    const request = ++latestRequest;
    if (id === null) {
        folderId.value = null;
        path.value = [];
        ready.value = true;
        return;
    }

    try {
        const response = await fileStore.folderPath(id);
        if (request !== latestRequest) return;
        path.value = response.path;
        folderId.value = id;
        ready.value = true;
    } catch (error) {
        if (request !== latestRequest) return;
        toastStore.errorResponseToast(error);
        await router.replace({ name: "dashboard" });
    }
}

async function onChanged(): Promise<void> {
    await filesRef.value?.refresh();
}

watch(routeFolderId, openFolder, { immediate: true });
</script>

<template>
    <div class="mx-auto flex w-full max-w-4xl flex-col gap-8 px-4 py-10">
        <div class="flex flex-col gap-1">
            <h1 class="sr-only">{{ folderName ?? "Files" }}</h1>
            <div class="flex items-center justify-between gap-3">
                <FolderBreadcrumb :path="path" />
                <Button
                    label="New folder"
                    icon="pi pi-folder-plus"
                    size="small"
                    severity="secondary"
                    variant="outlined"
                    class="shrink-0"
                    :disabled="!ready"
                    @click="newFolderVisible = true"
                />
            </div>
            <p class="text-sm text-fg-muted">Files are stored on this server's disk. Only you can see your files.</p>
        </div>

        <UploadDropzone :folder-id="folderId" :folder-name="folderName" @uploaded="onChanged" />

        <FilesPaginated v-if="ready" ref="filesRef" :folder-id="folderId" />

        <NewFolderDialog v-model:visible="newFolderVisible" :folder-id="folderId" @created="onChanged" />
    </div>
</template>
