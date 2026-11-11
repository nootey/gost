<script setup lang="ts">
import { ref } from "vue";
import { useRegle } from "@regle/core";
import { required } from "@regle/rules";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import ValidationError from "../../../pkg/components/ValidationError.vue";
import type { FolderForm, StoredFile } from "../models.ts";

const props = defineProps<{ folderId: number | null }>();
const visible = defineModel<boolean>("visible", { required: true });
const emit = defineEmits<{ created: [folder: StoredFile] }>();

const fileStore = useFileStore();
const toastStore = useToastStore();

const loading = ref<boolean>(false);

const form = ref<FolderForm>({
    name: "",
    parent_id: props.folderId,
});

const { r$ } = useRegle(form, {
    name: {
        required,
    },
});

// Start empty each time, inside whatever folder is open now.
function reset(): void {
    r$.$reset({ toState: { name: "", parent_id: props.folderId } });
}

// Enter can fire again while a create is running; a second call would make "Name (1)".
async function create(): Promise<void> {
    if (loading.value) return;
    await r$.$validate();
    if (r$.$invalid) return;

    loading.value = true;
    try {
        const response = await fileStore.createFolder(form.value);
        toastStore.successResponseToast(response);
        emit("created", response.folder);
        visible.value = false;
    } catch (error) {
        toastStore.errorResponseToast(error);
    } finally {
        loading.value = false;
    }
}
</script>

<template>
    <Dialog
        v-model:visible="visible"
        modal
        header="New folder"
        :draggable="false"
        class="w-[min(420px,calc(100vw-2rem))] rounded-2xl border-edge bg-panel"
        @show="reset"
    >
        <div class="flex flex-col gap-1">
            <ValidationError :is-required="true" :message="r$.name.$errors[0]">
                <label for="folder-name">Name</label>
            </ValidationError>
            <InputText
                id="folder-name"
                v-model="form.name"
                placeholder="Folder name"
                class="w-full rounded-xl"
                autofocus
                @keydown.enter="create"
            />
        </div>

        <template #footer>
            <Button label="Cancel" severity="secondary" variant="outlined" @click="visible = false" />
            <Button
                :label="loading ? 'Creating...' : 'Create'"
                :icon="loading ? 'pi pi-spin pi-spinner mr-2' : ''"
                :disabled="loading"
                @click="create"
            />
        </template>
    </Dialog>
</template>
