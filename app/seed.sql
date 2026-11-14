CREATE TABLE customers (id serial PRIMARY KEY, email text NOT NULL, created_at timestamptz DEFAULT now());
CREATE TABLE orders (id serial PRIMARY KEY, customer_id int REFERENCES customers(id), total numeric(10,2));
INSERT INTO customers (email) SELECT 'user' || g || '@example.com' FROM generate_series(1, 5000) g;
INSERT INTO orders (customer_id, total) SELECT (g % 5000) + 1, (random() * 200)::numeric(10,2) FROM generate_series(1, 40000) g;
