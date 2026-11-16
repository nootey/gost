<script setup lang="ts">
import { computed, ref } from "vue";
import { RouterLink } from "vue-router";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { isPicture, itemView } from "../../../pkg/utils/item_view.ts";
import type { RouteLocationRaw } from "vue-router";
import type { ItemView } from "../../../pkg/utils/item_view.ts";
import type { StoredFile } from "../models.ts";

// to makes the icon a link that opens the item, the same as its name.
const props = defineProps<{ item: StoredFile; to?: RouteLocationRaw | null }>();

const fileStore = useFileStore();

// The id whose thumbnail failed to load. A row can show another item later, so a failure counts only for its own id.
const failedId = ref<number | null>(null);

const view = computed<ItemView>(() => itemView(props.item));

const thumbnailUrl = computed<string | null>(() =>
    isPicture(props.item) && failedId.value !== props.item.id ? fileStore.thumbnailUrl(props.item.id) : null,
);
</script>

<template>
    <!-- The name link next to it takes the keyboard focus, so the icon link takes none. -->
    <component
        :is="to ? RouterLink : 'span'"
        v-bind="to ? { to, tabindex: -1, 'aria-hidden': true } : {}"
        class="grid size-9 shrink-0 place-items-center overflow-hidden rounded-lg"
        :class="view.iconBox"
    >
        <!-- A picture with no thumbnail (too big or broken) gets a 404, and the icon shows instead. -->
        <img
            v-if="thumbnailUrl"
            :src="thumbnailUrl"
            alt=""
            loading="lazy"
            decoding="async"
            draggable="false"
            class="size-full object-cover"
            @error="failedId = item.id"
        />
        <i v-else :class="view.icon" />
    </component>
</template>
