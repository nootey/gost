<script setup lang="ts">
// compact shows only the icon on a phone, where the header has no room for the name.
defineProps<{ compact?: boolean }>();
</script>

<template>
    <span class="flex items-center gap-2.5">
        <span class="grid size-8 place-items-center rounded-lg bg-primary text-primary-contrast shadow-sm">
            <svg viewBox="0 0 24 24" class="size-5" fill="currentColor" aria-hidden="true">
                <path
                    d="M9.5 2h-4a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h7a2 2 0 0 0 2-2V8.25h-2.75a2.25 2.25 0 0 1-2.25-2.25V2Z"
                />
                <path d="M10.75 2.6l3.15 4.25h-2.15a1 1 0 0 1-1-1Z" />
                <path
                    fill-rule="evenodd"
                    d="M15.75 4.06A4.5 4.5 0 0 1 21 8.5v8a1.5 1 0 0 1-3 0 1.5 1 0 0 0-2.25.87ZM16.45 8.9a.75 1.05 0 1 0 1.5 0 .75 1.05 0 1 0-1.5 0Zm2.05 0a.75 1.05 0 1 0 1.5 0 .75 1.05 0 1 0-1.5 0Z"
                />
            </svg>
        </span>
        <span class="text-lg font-semibold tracking-tight text-fg" :class="{ 'hidden sm:inline': compact }">gost</span>
    </span>
</template>
