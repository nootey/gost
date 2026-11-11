<script setup lang="ts">
import { computed, ref } from "vue";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import type { FileUploadUploaderEvent } from "primevue/fileupload";
import type { StoredFile } from "../models.ts";

const props = defineProps<{ folderId: number | null; folderName?: string }>();
const emit = defineEmits<{ uploaded: [file: StoredFile] }>();

const fileStore = useFileStore();
const toastStore = useToastStore();

const uploadRef = ref<{ choose: () => void; clear: () => void } | null>(null);
const current = ref<File | null>(null);
const remaining = ref<number>(0);

const busy = computed<boolean>(() => current.value !== null);

function browse(): void {
    uploadRef.value?.choose();
}

// Files go up one at a time, so each one gets its own result from the API.
async function onUpload(event: FileUploadUploaderEvent): Promise<void> {
    const files = Array.isArray(event.files) ? [...event.files] : [event.files];
    uploadRef.value?.clear();
    // Keep the folder from the drop, so opening another folder mid-batch does not split the batch.
    const folderId = props.folderId;

    remaining.value = files.length;
    for (const file of files) {
        current.value = file;
        try {
            const response = await fileStore.upload(file, folderId);
            toastStore.successResponseToast(response);
            emit("uploaded", response.file);
        } catch (error) {
            toastStore.errorResponseToast(error);
        } finally {
            remaining.value--;
        }
    }
    current.value = null;
}
</script>

<template>
    <FileUpload
        ref="uploadRef"
        name="file"
        unstyled
        :multiple="true"
        :auto="true"
        custom-upload
        :disabled="busy"
        :pt="{ input: { class: 'hidden' }, header: { class: 'hidden' }, content: { class: 'group' } }"
        @uploader="onUpload"
    >
        <template #content>
            <div
                class="flex flex-col items-center justify-center gap-4 rounded-2xl border-2 border-dashed border-edge bg-panel px-6 py-14 text-center transition-colors group-data-[p-highlight=true]:border-accent group-data-[p-highlight=true]:bg-accent-soft"
            >
                <template v-if="busy">
                    <i class="pi pi-spin pi-spinner text-2xl text-accent" />
                    <div class="flex max-w-full flex-col gap-1">
                        <span class="truncate font-medium text-fg">Uploading {{ current?.name }}</span>
                        <span v-if="remaining > 1" class="text-sm text-fg-muted">
                            {{ remaining - 1 }} more after this one
                        </span>
                    </div>
                    <ProgressBar mode="indeterminate" class="h-1 w-48" />
                </template>

                <template v-else>
                    <span class="grid size-14 place-items-center rounded-2xl bg-accent-soft text-accent">
                        <i class="pi pi-cloud-upload text-2xl" />
                    </span>
                    <div class="flex flex-col gap-1">
                        <span class="text-base font-medium text-fg">Drop files here to upload</span>
                        <span class="text-sm text-fg-muted">
                            <template v-if="folderName">
                                Files go into
                                <span class="font-medium text-fg">{{ folderName }}</span>
                            </template>
                            <template v-else>You can add more than one file at a time.</template>
                        </span>
                    </div>
                    <Button label="Browse files" icon="pi pi-folder-open" size="small" @click="browse" />
                </template>
            </div>
        </template>
    </FileUpload>
</template>
