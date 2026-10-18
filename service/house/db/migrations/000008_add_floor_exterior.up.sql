-- exterior marks a floor as an outdoor area. Pre-migration floors default to
-- 0 (interior), which is the safe reading until someone flags them.
ALTER TABLE floor ADD COLUMN exterior INT NOT NULL DEFAULT 0;
